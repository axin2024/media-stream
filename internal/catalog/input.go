package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Blob struct {
	Key    string   `json:"-"`
	Digest [32]byte `json:"-"`
	Bytes  int64    `json:"bytes"`
}

func registerBlob(ctx context.Context, tx *sql.Tx, blob Blob) error {
	if blob.Key == "" || blob.Bytes < 1 {
		return ErrInvalid
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO blobs(blob_key,digest,bytes) VALUES(?,?,?)", blob.Key, blob.Digest[:], blob.Bytes)
	return err
}

type Fragment struct {
	SourceMS   int64 `json:"source_ms"`
	DurationMS int   `json:"duration_ms"`
	OffsetMS   int64 `json:"offset_ms"`
	Replay     bool  `json:"replay"`
}

func (s *Store) Ingest(ctx context.Context, caller Identity, id string, sourceMS int64, durationMS int, blob Blob) (Fragment, error) {
	if caller.Role != "push" || caller.Session != id || !validSource(caller.Source) {
		return Fragment{}, ErrDenied
	}
	if sourceMS < 0 || durationMS < 100 {
		return Fragment{}, ErrInvalid
	}
	return transaction(ctx, s, func(tx *sql.Tx) (Fragment, error) {
		stream, err := readSession(ctx, tx, id, true)
		if err != nil {
			return Fragment{}, err
		}
		owned, err := productionOwnsSession(ctx, tx, id)
		if err != nil || owned {
			if owned {
				err = ErrFenced
			}
			return Fragment{}, err
		}
		var inputGeneration uint64
		var origin int64
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT i.generation,i.clock_origin_ms,
			EXISTS (SELECT 1 FROM credentials c WHERE c.session_id=i.session_id AND c.source=i.source
				AND c.source_generation=i.generation AND c.role='push' AND c.revoked_at IS NULL AND c.expires_at>UTC_TIMESTAMP(6))
			FROM source_inputs i WHERE i.session_id=? AND i.source=?`, id, caller.Source).Scan(&inputGeneration, &origin, &active)
		if err != nil {
			return Fragment{}, missing(err)
		}
		if !active || inputGeneration != caller.SourceGeneration {
			return Fragment{}, ErrFenced
		}
		offset := sourceMS - origin
		if offset < 0 || offset > 172_800_000 || durationMS > stream.SegmentMS+250 || offset%int64(stream.SegmentMS) != 0 {
			return Fragment{}, ErrInvalid
		}
		durationMS = min(durationMS, stream.SegmentMS)
		result := Fragment{SourceMS: sourceMS, OffsetMS: offset, DurationMS: durationMS}
		var oldDuration int
		var prior []byte
		err = tx.QueryRowContext(ctx, `SELECT i.duration_ms,b.digest FROM input_fragments i JOIN blobs b ON b.blob_key=i.blob_key
			WHERE i.session_id=? AND i.source=? AND i.generation=? AND i.source_ms=?`, id, caller.Source, inputGeneration, sourceMS).
			Scan(&oldDuration, &prior)
		if err == nil {
			if oldDuration != durationMS || !bytes.Equal(blob.Digest[:], prior) {
				return Fragment{}, ErrConflict
			}
			result.Replay = true
			return result, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Fragment{}, err
		}
		if stream.State == "archived" || stream.State == "ending" || offset < stream.NextOffsetMS {
			return Fragment{}, ErrFenced
		}
		// Bound source clock skew and the number of enqueued files per input.
		if offset > stream.NextOffsetMS+int64(8*stream.SegmentMS) {
			return Fragment{}, ErrBackpressure
		}
		if err := registerBlob(ctx, tx, blob); err != nil {
			return Fragment{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO input_fragments(session_id,source,generation,source_ms,offset_ms,duration_ms,blob_key)
			VALUES(?,?,?,?,?,?,?)`, id, caller.Source, inputGeneration, sourceMS, offset, durationMS, blob.Key); err != nil {
			return Fragment{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE source_inputs SET last_received_at=UTC_TIMESTAMP(6),reported_lost_at=NULL
			WHERE session_id=? AND source=?`, id, caller.Source); err != nil {
			return Fragment{}, err
		}
		return result, addUsage(ctx, tx, id, blob.Bytes, 0, 0, 0)
	})
}

func (s *Store) InputLost(ctx context.Context, caller Identity, id string) error {
	if caller.Role != "push" || caller.Session != id {
		return ErrDenied
	}
	_, err := transaction(ctx, s, func(tx *sql.Tx) (struct{}, error) {
		stream, err := readSession(ctx, tx, id, true)
		if err != nil {
			return struct{}{}, err
		}
		owned, err := productionOwnsSession(ctx, tx, id)
		if err != nil {
			return struct{}{}, err
		}
		if owned {
			return struct{}{}, ErrFenced
		}
		if stream.State == "ending" || stream.State == "archived" {
			return struct{}{}, ErrFenced
		}
		result, err := tx.ExecContext(ctx, `UPDATE source_inputs SET reported_lost_at=UTC_TIMESTAMP(6) WHERE session_id=?
			AND source=? AND generation=? AND reported_lost_at IS NULL`, id, caller.Source, caller.SourceGeneration)
		if err != nil {
			return struct{}{}, err
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return struct{}{}, err
		}
		return struct{}{}, audit(ctx, tx, id, "input_lost", caller.Label, map[string]any{
			"source": caller.Source, "generation": caller.SourceGeneration, "watermark_ms": stream.NextOffsetMS})
	})
	return err
}

type Rotation struct {
	Generation uint64 `json:"generation"`
	OriginMS   int64  `json:"source_zero_ms"`
	ExpiresIn  int    `json:"push_ttl_seconds"`
}

func (s *Store) RotateInput(ctx context.Context, caller Identity, id, source, idempotency string, input Rotation) (Grant, error) {
	if !validSource(source) || !requestKey.MatchString(idempotency) || input.Generation < 1 || input.OriginMS < 0 ||
		input.ExpiresIn < 30 || input.ExpiresIn > 86_400 || len(s.signing) < 32 {
		return Grant{}, ErrInvalid
	}
	fingerprint := digest(struct {
		Session, Source string
		Rotation
	}{id, source, input})
	return transaction(ctx, s, func(tx *sql.Tx) (Grant, error) {
		stream, err := readSession(ctx, tx, id, true)
		if err != nil {
			return Grant{}, err
		}
		if !caller.Allows(stream.Tenant, stream.Channel, id, "owner", "director") {
			return Grant{}, ErrMissing
		}
		owned, err := productionOwnsSession(ctx, tx, id)
		if err != nil {
			return Grant{}, err
		}
		if owned {
			return Grant{}, ErrConflict
		}
		if stream.State == "ending" || stream.State == "archived" {
			return Grant{}, ErrConflict
		}
		_, found, err := receipt(ctx, tx, caller.Tenant, "rotate", idempotency, fingerprint)
		if err != nil {
			return Grant{}, err
		}
		var generation uint64
		if err := tx.QueryRowContext(ctx, "SELECT generation FROM source_inputs WHERE session_id=? AND source=? FOR UPDATE",
			id, source).Scan(&generation); err != nil {
			return Grant{}, err
		}
		if found {
			if generation != input.Generation+1 {
				return Grant{}, ErrFenced
			}
			secret := s.pushSecret(id, source, generation)
			sum := digestSecret(secret)
			var expires time.Time
			err := tx.QueryRowContext(ctx, "SELECT expires_at FROM credentials WHERE digest=? AND revoked_at IS NULL AND expires_at>UTC_TIMESTAMP(6)",
				sum[:]).Scan(&expires)
			return Grant{Secret: secret, Generation: generation, Expires: expires}, missing(err)
		}
		var until, now time.Time
		if err := tx.QueryRowContext(ctx, "SELECT available_until,UTC_TIMESTAMP(6) FROM channels WHERE tenant=? AND channel=?",
			stream.Tenant, stream.Channel).Scan(&until, &now); err != nil {
			return Grant{}, err
		}
		if generation != input.Generation || !now.Before(until) {
			return Grant{}, ErrFenced
		}
		if _, err := tx.ExecContext(ctx, `UPDATE credentials SET revoked_at=? WHERE session_id=? AND source=? AND role='push' AND revoked_at IS NULL`,
			now, id, source); err != nil {
			return Grant{}, err
		}
		generation++
		if _, err := tx.ExecContext(ctx, `UPDATE source_inputs SET generation=?,clock_origin_ms=?,last_received_at=NULL,reported_lost_at=NULL
			WHERE session_id=? AND source=?`, generation, input.OriginMS, id, source); err != nil {
			return Grant{}, err
		}
		if stream.ActiveSource == source {
			if err := supersede(ctx, tx, id); err != nil {
				return Grant{}, err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE sessions SET source_epoch=source_epoch+1,state='switching',revision=revision+1 WHERE id=?", id); err != nil {
				return Grant{}, err
			}
		}
		expires := now.Add(time.Duration(input.ExpiresIn) * time.Second)
		if expires.After(until) {
			expires = until
		}
		if err := audit(ctx, tx, id, "input_rotated", caller.Label, map[string]any{"source": source, "generation": generation}); err != nil {
			return Grant{}, err
		}
		if err := saveReceipt(ctx, tx, caller.Tenant, "rotate", idempotency, fingerprint, id); err != nil {
			return Grant{}, err
		}
		return s.newPushGrant(ctx, tx, stream.Tenant, stream.Channel, id, source, generation, expires)
	})
}

func (s *Store) PreferInput(ctx context.Context, caller Identity, id, source string, revision uint64) (Session, error) {
	if !validSource(source) || revision < 1 {
		return Session{}, ErrInvalid
	}
	return transaction(ctx, s, func(tx *sql.Tx) (Session, error) {
		stream, err := readSession(ctx, tx, id, true)
		if err != nil {
			return Session{}, err
		}
		if !caller.Allows(stream.Tenant, stream.Channel, id, "owner", "director") {
			return Session{}, ErrMissing
		}
		owned, err := productionOwnsSession(ctx, tx, id)
		if err != nil {
			return Session{}, err
		}
		if owned {
			return Session{}, ErrConflict
		}
		if stream.Revision != revision || stream.State == "ending" || stream.State == "archived" {
			return Session{}, ErrConflict
		}
		if _, err := tx.ExecContext(ctx, "UPDATE sessions SET preferred_source=?,revision=revision+1 WHERE id=?", source, id); err != nil {
			return Session{}, err
		}
		if err := audit(ctx, tx, id, "input_requested", caller.Label, map[string]string{"source": source}); err != nil {
			return Session{}, err
		}
		return readSession(ctx, tx, id, false)
	})
}

func supersede(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, "UPDATE transcode_jobs SET state='superseded' WHERE pending_session=?", id)
	return err
}

func addUsage(ctx context.Context, tx *sql.Tx, id string, ingress, egress, requests, failures int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_hourly(session_id,hour_start,ingress_bytes,egress_bytes,media_requests,egress_errors)
		VALUES(?,DATE_FORMAT(UTC_TIMESTAMP(),'%Y-%m-%d %H:00:00'),?,?,?,?) AS delta
		ON DUPLICATE KEY UPDATE ingress_bytes=usage_hourly.ingress_bytes+delta.ingress_bytes,
			egress_bytes=usage_hourly.egress_bytes+delta.egress_bytes,media_requests=usage_hourly.media_requests+delta.media_requests,
			egress_errors=usage_hourly.egress_errors+delta.egress_errors`, id, ingress, egress, requests, failures)
	return err
}

func (s *Store) Egress(ctx context.Context, id string, count int64, failed bool) error {
	var failedCount int64
	if failed {
		failedCount = 1
	}
	_, err := transaction(ctx, s, func(tx *sql.Tx) (struct{}, error) {
		return struct{}{}, addUsage(ctx, tx, id, 0, count, 1, failedCount)
	})
	return err
}

func (s *Store) FormatSource(ctx context.Context, caller Identity, id string) (int, error) {
	if caller.Role != "push" || caller.Session != id {
		return 0, ErrDenied
	}
	var duration int
	err := s.db.QueryRowContext(ctx, `SELECT segment_ms FROM sessions s WHERE s.id=? AND s.tenant=?
		AND NOT EXISTS(SELECT 1 FROM productions p WHERE p.session_id=s.id)`, id, caller.Tenant).Scan(&duration)
	if err != nil {
		return 0, fmt.Errorf("read input format: %w", missing(err))
	}
	return duration, nil
}
