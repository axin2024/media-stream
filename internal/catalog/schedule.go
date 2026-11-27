package catalog

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const leaseSeconds = 30
const maxAttempts = 3

type inputCandidate struct {
	Source     string
	Generation uint64
	SourceMS   int64
	OffsetMS   int64
	DurationMS int
	BlobKey    string
	Fresh      bool
}

func (s *Store) PendingSessions(ctx context.Context, after string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id FROM sessions s WHERE s.id>? AND s.state<>'archived'
		AND NOT EXISTS(SELECT 1 FROM productions p WHERE p.session_id=s.id) ORDER BY s.id LIMIT 128`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func candidate(ctx context.Context, tx *sql.Tx, stream Session, source string, freshnessUsec int64) (inputCandidate, error) {
	result := inputCandidate{Source: source}
	err := tx.QueryRowContext(ctx, `SELECT s.generation,s.last_received_at>TIMESTAMPADD(MICROSECOND,-?,UTC_TIMESTAMP(6))
		AND s.reported_lost_at IS NULL,i.source_ms,i.offset_ms,i.duration_ms,i.blob_key
		FROM input_fragments i JOIN source_inputs s ON s.session_id=i.session_id AND s.source=i.source AND s.generation=i.generation
		WHERE i.session_id=? AND i.source=? AND i.offset_ms>=? AND NOT EXISTS(
			SELECT 1 FROM transcode_jobs j WHERE j.session_id=i.session_id AND j.seq=? AND j.source=i.source
			AND j.source_generation=i.generation AND j.source_ms=i.source_ms AND j.state='failed')
		ORDER BY i.offset_ms LIMIT 1`, freshnessUsec, stream.ID, source, stream.NextOffsetMS, stream.NextSeq).
		Scan(&result.Generation, &result.Fresh, &result.SourceMS, &result.OffsetMS, &result.DurationMS, &result.BlobKey)
	if errors.Is(err, sql.ErrNoRows) {
		return inputCandidate{Source: source}, nil
	}
	return result, err
}

func chooseInput(stream Session, primary, backup inputCandidate, activeFresh bool, firstWait bool) (inputCandidate, string) {
	ending := stream.State == "ending"
	active, alternate := primary, backup
	if stream.ActiveSource == "backup" {
		active, alternate = backup, primary
	}
	if stream.PreferredSource != "" {
		preferred := primary
		if stream.PreferredSource == "backup" {
			preferred = backup
		}
		if preferred.BlobKey != "" && (preferred.Fresh || ending) {
			return preferred, "director"
		}
	}
	if stream.ActiveSource == "" {
		if primary.BlobKey != "" && (primary.Fresh || ending || !firstWait) {
			return primary, "initial"
		}
		if backup.BlobKey != "" && !firstWait {
			return backup, "primary_unavailable"
		}
		return inputCandidate{}, ""
	}
	if active.BlobKey != "" && active.OffsetMS == stream.NextOffsetMS && (activeFresh || ending) {
		return active, "continuity"
	}
	if alternate.BlobKey != "" && (alternate.Fresh || ending) &&
		(ending || !activeFresh || (active.BlobKey != "" && alternate.OffsetMS < active.OffsetMS)) {
		return alternate, "active_unavailable"
	}
	if active.BlobKey != "" && (!activeFresh || ending) {
		return active, "buffered"
	}
	return inputCandidate{}, ""
}

func (s *Store) Schedule(ctx context.Context, id string) error {
	_, err := transaction(ctx, s, func(tx *sql.Tx) (struct{}, error) {
		var zero struct{}
		stream, err := readSession(ctx, tx, id, true)
		if err != nil || stream.State == "archived" {
			return zero, err
		}
		owned, err := productionOwnsSession(ctx, tx, id)
		if err != nil || owned {
			return zero, err
		}
		var now, until time.Time
		if err := tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6),available_until FROM channels WHERE tenant=? AND channel=?",
			stream.Tenant, stream.Channel).Scan(&now, &until); err != nil {
			return zero, err
		}
		if !now.Before(until) && stream.State != "ending" {
			if err := markEnding(ctx, tx, id, "scheduler"); err != nil {
				return zero, err
			}
			stream.State = "ending"
		}
		validFor := time.Duration(stream.SegmentMS+1_250) * time.Millisecond
		primary, err := candidate(ctx, tx, stream, "primary", validFor.Microseconds())
		if err != nil {
			return zero, err
		}
		backup, err := candidate(ctx, tx, stream, "backup", validFor.Microseconds())
		if err != nil {
			return zero, err
		}
		var activeFresh bool
		if stream.ActiveSource != "" {
			err = tx.QueryRowContext(ctx, `SELECT COALESCE(last_received_at>?,false) AND reported_lost_at IS NULL
				FROM source_inputs WHERE session_id=? AND source=?`, now.Add(-validFor), id, stream.ActiveSource).Scan(&activeFresh)
			if err != nil {
				return zero, err
			}
		}
		next, reason := chooseInput(stream, primary, backup, activeFresh, now.Sub(stream.StartedAt) < validFor && stream.State != "ending")
		var pending uint64
		err = tx.QueryRowContext(ctx, "SELECT id FROM transcode_jobs WHERE pending_session=? FOR UPDATE", id).Scan(&pending)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return zero, err
		}
		if pending != 0 {
			if next.BlobKey == "" || next.Source == stream.ActiveSource || stream.State == "ending" ||
				(reason != "director" && activeFresh) {
				return zero, nil
			}
			if err := supersede(ctx, tx, id); err != nil {
				return zero, err
			}
		}
		if next.BlobKey == "" {
			if stream.State == "ending" {
				return zero, archive(ctx, tx, stream)
			}
			if stream.ActiveSource != "" && !activeFresh && stream.State != "switching" {
				if _, err := tx.ExecContext(ctx, "UPDATE sessions SET state='switching',revision=revision+1 WHERE id=?", id); err != nil {
					return zero, err
				}
				return zero, audit(ctx, tx, id, "input_timeout", "scheduler", map[string]any{
					"source": stream.ActiveSource, "watermark_ms": stream.NextOffsetMS})
			}
			return zero, nil
		}
		if next.Source != stream.ActiveSource {
			stream.SourceEpoch++
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET active_source=?,source_epoch=?,source_floor_ms=?,
				preferred_source=IF(preferred_source=?,'',preferred_source),state=IF(state='ending',state,'switching'),
				revision=revision+1 WHERE id=?`, next.Source, stream.SourceEpoch, next.OffsetMS, next.Source, id); err != nil {
				return zero, err
			}
			if err := audit(ctx, tx, id, "input_selected", "scheduler", map[string]any{
				"previous": stream.ActiveSource, "source": next.Source, "reason": reason, "source_epoch": stream.SourceEpoch,
				"offset_ms": next.OffsetMS, "source_ms": next.SourceMS, "watermark_ms": stream.NextOffsetMS}); err != nil {
				return zero, err
			}
		} else if stream.PreferredSource == next.Source {
			if _, err := tx.ExecContext(ctx, "UPDATE sessions SET preferred_source='' WHERE id=?", id); err != nil {
				return zero, err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO transcode_jobs
			(session_id,source,source_generation,source_epoch,source_ms,offset_ms,duration_ms,seq,gap_ms,input_blob)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, id, next.Source, next.Generation, stream.SourceEpoch, next.SourceMS, next.OffsetMS,
			next.DurationMS, stream.NextSeq, next.OffsetMS-stream.SourceEndMS, next.BlobKey)
		return zero, err
	})
	return err
}

func markEnding(ctx context.Context, tx *sql.Tx, id, actor string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state='ending',ended_at=UTC_TIMESTAMP(6),
		preferred_source='',revision=revision+1 WHERE id=?`, id); err != nil {
		return err
	}
	return audit(ctx, tx, id, "session_ending", actor, struct{}{})
}

func (s *Store) End(ctx context.Context, caller Identity, id string) (Session, error) {
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
		if stream.State != "ending" && stream.State != "archived" {
			if err := markEnding(ctx, tx, id, caller.Label); err != nil {
				return Session{}, err
			}
			return readSession(ctx, tx, id, false)
		}
		return stream, nil
	})
}

func archive(ctx context.Context, tx *sql.Tx, stream Session) error {
	state := "published"
	if stream.NextSeq == 0 {
		state = "empty"
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO recordings(id,session_id,state,duration_ms) VALUES(?,?,?,?)",
		stream.ID, stream.ID, state, stream.MediaDurationMS); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET state='archived',revision=revision+1 WHERE id=?", stream.ID); err != nil {
		return err
	}
	return audit(ctx, tx, stream.ID, "recording_archived", "scheduler", map[string]any{
		"segments": stream.NextSeq, "media_duration_ms": stream.MediaDurationMS, "state": state})
}
