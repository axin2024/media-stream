package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type Profile struct {
	Track     string
	Width     int
	Height    int
	VideoKbps int
	AudioKbps int
}

func Profiles(preset string) []Profile {
	if preset == "low-bandwidth" {
		return []Profile{{"240p", 426, 240, 400, 64}, {"360p", 640, 360, 850, 96}}
	}
	return []Profile{{"360p", 640, 360, 850, 96}, {"720p", 1280, 720, 2_300, 128}}
}

type Job struct {
	ID           uint64
	Session      string
	Preset       string
	Source       string
	SourceGen    uint64
	SourceEpoch  uint64
	SourceMS     int64
	OffsetMS     int64
	DurationMS   int
	MediaStartMS int64
	SegmentMS    int
	Seq          uint64
	GapMS        int64
	Input        Blob
	Node         string
	Attempt      int
}

func (s *Store) RegisterNode(ctx context.Context, node string, slots int, enabled bool) error {
	if !identifier.MatchString(node) || slots < 1 || slots > 32 {
		return ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO transcode_nodes(id,slots,enabled) VALUES(?,?,?) AS proposed
		ON DUPLICATE KEY UPDATE slots=proposed.slots,enabled=proposed.enabled`, node, slots, enabled)
	return err
}

func (s *Store) CheckNode(ctx context.Context, node string) error {
	var id string
	return missing(s.db.QueryRowContext(ctx, "SELECT id FROM transcode_nodes WHERE id=?", node).Scan(&id))
}

func nodeCapacity(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var slots int
	var enabled bool
	if err := tx.QueryRowContext(ctx, "SELECT slots,enabled FROM transcode_nodes WHERE id=? FOR UPDATE", name).Scan(&slots, &enabled); err != nil {
		return false, missing(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE transcode_nodes SET last_seen=UTC_TIMESTAMP(6) WHERE id=?", name); err != nil {
		return false, err
	}
	var used int
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM transcode_jobs WHERE node=? AND state='running' AND lease_until>UTC_TIMESTAMP(6))+
		(SELECT COUNT(*) FROM edits WHERE node=? AND state='running' AND lease_until>UTC_TIMESTAMP(6))`, name, name).Scan(&used)
	return enabled && used < slots, err
}

func (s *Store) Claim(ctx context.Context, node string) (*Job, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (*Job, error) {
		capacity, err := nodeCapacity(ctx, tx, node)
		if err != nil || !capacity {
			return nil, err
		}
		var job Job
		var attempts int
		err = tx.QueryRowContext(ctx, `SELECT id,attempts FROM transcode_jobs
			WHERE state='queued' OR (state='running' AND lease_until<=UTC_TIMESTAMP(6))
			ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&job.ID, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if attempts >= maxAttempts {
			_, err := tx.ExecContext(ctx, "UPDATE transcode_jobs SET state='failed',error='lease attempts exhausted' WHERE id=?", job.ID)
			return nil, err
		}
		job.Node, job.Attempt = node, attempts+1
		var digest []byte
		err = tx.QueryRowContext(ctx, `SELECT j.session_id,s.preset,j.source,j.source_generation,j.source_epoch,j.source_ms,
			j.offset_ms,j.duration_ms,s.media_duration_ms,s.segment_ms,j.seq,j.gap_ms,b.blob_key,b.digest,b.bytes
			FROM transcode_jobs j JOIN sessions s ON s.id=j.session_id JOIN blobs b ON b.blob_key=j.input_blob WHERE j.id=?`, job.ID).
			Scan(&job.Session, &job.Preset, &job.Source, &job.SourceGen, &job.SourceEpoch, &job.SourceMS, &job.OffsetMS,
				&job.DurationMS, &job.MediaStartMS, &job.SegmentMS, &job.Seq, &job.GapMS, &job.Input.Key, &digest, &job.Input.Bytes)
		if err != nil {
			return nil, err
		}
		copy(job.Input.Digest[:], digest)
		_, err = tx.ExecContext(ctx, `UPDATE transcode_jobs SET node=?,state='running',attempts=?,error=NULL,
			lease_until=TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)) WHERE id=?`, node, job.Attempt, leaseSeconds, job.ID)
		return &job, err
	})
}

func (s *Store) Renew(ctx context.Context, job Job) error {
	result, err := s.db.ExecContext(ctx, `UPDATE transcode_jobs SET lease_until=TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6))
		WHERE id=? AND node=? AND attempts=? AND state='running' AND lease_until>UTC_TIMESTAMP(6)`,
		leaseSeconds, job.ID, job.Node, job.Attempt)
	return fencedResult(result, err)
}

func fencedResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return ErrFenced
	}
	return err
}

type Rendition struct {
	Track string
	Blob  Blob
}

func (s *Store) Complete(ctx context.Context, job Job, output []Rendition, report string) error {
	valid := make(map[string]bool)
	for _, profile := range Profiles(job.Preset) {
		valid[profile.Track] = true
	}
	for _, track := range output {
		if !valid[track.Track] {
			return ErrInvalid
		}
		delete(valid, track.Track)
	}
	_, err := transaction(ctx, s, func(tx *sql.Tx) (struct{}, error) {
		var zero struct{}
		stream, err := readSession(ctx, tx, job.Session, true)
		if err != nil {
			return zero, err
		}
		if stream.SourceEpoch != job.SourceEpoch || stream.NextSeq != job.Seq || stream.ActiveSource != job.Source ||
			stream.MediaDurationMS != job.MediaStartMS || stream.State == "archived" || job.OffsetMS < stream.SourceFloorMS {
			return zero, ErrFenced
		}
		var generation uint64
		if err := tx.QueryRowContext(ctx, "SELECT generation FROM source_inputs WHERE session_id=? AND source=?",
			job.Session, job.Source).Scan(&generation); err != nil {
			return zero, err
		}
		if generation != job.SourceGen {
			return zero, ErrFenced
		}
		state := "done"
		if len(output) == 0 {
			state = "queued"
			if job.Attempt >= maxAttempts {
				state = "failed"
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE transcode_jobs SET state=?,error=NULLIF(?,''),lease_until=NULL
			WHERE id=? AND session_id=? AND node=? AND attempts=? AND state='running' AND lease_until>UTC_TIMESTAMP(6)`,
			state, shortDiagnostic(report), job.ID, job.Session, job.Node, job.Attempt)
		if err := fencedResult(result, err); err != nil {
			return zero, err
		}
		if len(output) == 0 {
			return zero, audit(ctx, tx, job.Session, "transcode_failed", job.Node, map[string]any{
				"job": job.ID, "attempt": job.Attempt, "state": state, "sequence": job.Seq, "reason": shortDiagnostic(report)})
		}
		for _, track := range output {
			if err := registerBlob(ctx, tx, track.Blob); err != nil {
				return zero, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO segments
				(session_id,track,seq,source,source_epoch,source_ms,offset_ms,media_ms,duration_ms,gap_ms,blob_key)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`, job.Session, track.Track, job.Seq, job.Source, job.SourceEpoch, job.SourceMS,
				job.OffsetMS, job.MediaStartMS, job.DurationMS, job.GapMS, track.Blob.Key); err != nil {
				return zero, err
			}
		}
		liveState := "live"
		if len(valid) != 0 {
			liveState = "degraded"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state=IF(state='ending',state,?),next_seq=next_seq+1,
			next_offset_ms=?,source_end_ms=?,media_duration_ms=media_duration_ms+?,revision=revision+1 WHERE id=?`,
			liveState, job.OffsetMS+int64(stream.SegmentMS), job.OffsetMS+int64(job.DurationMS), job.DurationMS, job.Session); err != nil {
			return zero, err
		}
		return zero, audit(ctx, tx, job.Session, "segment_committed", job.Node, map[string]any{
			"job": job.ID, "attempt": job.Attempt, "seq": job.Seq, "source": job.Source, "source_epoch": job.SourceEpoch,
			"offset_ms": job.OffsetMS, "media_ms": job.MediaStartMS, "gap_ms": job.GapMS, "tracks": len(output),
			"missing_tracks": valid, "state": liveState, "reason": shortDiagnostic(report)})
	})
	return err
}

func shortDiagnostic(input string) string {
	words := []rune(strings.Join(strings.Fields(input), " "))
	if len(words) > 240 {
		return string(words[:240])
	}
	return string(words)
}

type Node struct {
	ID      string `json:"id"`
	Slots   int    `json:"slots"`
	Enabled bool   `json:"enabled"`
	SeenAt  string `json:"last_seen"`
}

func (s *Store) Nodes(ctx context.Context, caller Identity) ([]Node, error) {
	if caller.Role != "operator" || caller.Channel != "" {
		return nil, ErrDenied
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,slots,enabled,
		DATE_FORMAT(last_seen,'%Y-%m-%dT%H:%i:%sZ') FROM transcode_nodes ORDER BY id LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := make([]Node, 0)
	for rows.Next() {
		var node Node
		if err := rows.Scan(&node.ID, &node.Slots, &node.Enabled, &node.SeenAt); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}
