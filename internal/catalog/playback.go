package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type Segment struct {
	Seq         uint64    `json:"sequence"`
	Track       string    `json:"track"`
	Source      string    `json:"source"`
	SourceEpoch uint64    `json:"source_epoch"`
	SourceMS    int64     `json:"source_ms"`
	OffsetMS    int64     `json:"offset_ms"`
	MediaMS     int64     `json:"media_ms"`
	DurationMS  int       `json:"duration_ms"`
	GapMS       int64     `json:"gap_ms"`
	Published   time.Time `json:"published_at"`
	Blob        Blob      `json:"blob"`
}

type Recording struct {
	ID           string     `json:"id"`
	Session      string     `json:"session_id"`
	Channel      string     `json:"channel"`
	State        string     `json:"state"`
	Revision     uint64     `json:"revision"`
	DurationMS   int64      `json:"duration_ms"`
	CreatedAt    time.Time  `json:"created_at"`
	VisibleUntil *time.Time `json:"visible_until,omitempty"`
	Title        string     `json:"channel_title"`
}

const recordingSelect = `SELECT r.id,r.session_id,s.channel,r.state,r.revision,r.duration_ms,r.created_at,r.visible_until,c.title
	FROM recordings r JOIN sessions s ON s.id=r.session_id JOIN channels c ON c.tenant=s.tenant AND c.channel=s.channel`

func readRecording(ctx context.Context, db queryer, id string, caller Identity, public bool, lock bool) (Recording, error) {
	query := recordingSelect + " WHERE s.tenant=? AND r.id=? AND (?='' OR s.channel=?) AND (?='' OR s.id=?)"
	if public || caller.Role == "viewer" {
		query += " AND r.state='published' AND (r.visible_until IS NULL OR r.visible_until>UTC_TIMESTAMP(6))"
	}
	if lock {
		query += " FOR UPDATE OF r"
	}
	var rec Recording
	err := db.QueryRowContext(ctx, query, caller.Tenant, id, caller.Channel, caller.Channel, caller.Session, caller.Session).
		Scan(&rec.ID, &rec.Session, &rec.Channel, &rec.State, &rec.Revision, &rec.DurationMS, &rec.CreatedAt, &rec.VisibleUntil, &rec.Title)
	return rec, missing(err)
}

func (s *Store) Recordings(ctx context.Context, caller Identity, before string) ([]Recording, error) {
	if caller.Role == "push" {
		return nil, ErrDenied
	}
	query := recordingSelect + " WHERE s.tenant=? AND (?='' OR s.channel=?) AND (?='' OR s.id=?)"
	args := []any{caller.Tenant, caller.Channel, caller.Channel, caller.Session, caller.Session}
	if before != "" {
		cursor, err := readRecording(ctx, s.db, before, caller, false, false)
		if err != nil {
			return nil, err
		}
		query += " AND (r.created_at<? OR (r.created_at=? AND r.id<?))"
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	if caller.Role == "viewer" {
		query += " AND r.state='published' AND (r.visible_until IS NULL OR r.visible_until>UTC_TIMESTAMP(6))"
	}
	query += " ORDER BY r.created_at DESC,r.id DESC LIMIT 101"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Recording, 0)
	for rows.Next() {
		var rec Recording
		if err := rows.Scan(&rec.ID, &rec.Session, &rec.Channel, &rec.State, &rec.Revision, &rec.DurationMS,
			&rec.CreatedAt, &rec.VisibleUntil, &rec.Title); err != nil {
			return nil, err
		}
		result = append(result, rec)
	}
	return result, rows.Err()
}

func (s *Store) Recording(ctx context.Context, caller Identity, id string) (Recording, error) {
	if caller.Role == "push" {
		return Recording{}, ErrDenied
	}
	return readRecording(ctx, s.db, id, caller, false, false)
}

func (s *Store) Playback(ctx context.Context, caller Identity, id string, recorded bool) (Session, error) {
	stream, err := s.Session(ctx, caller, id)
	if err != nil {
		return Session{}, err
	}
	if recorded && stream.State != "archived" {
		return Session{}, ErrMissing
	}
	if stream.State == "archived" {
		if _, err := readRecording(ctx, s.db, id, caller, true, false); err != nil {
			return Session{}, err
		}
	}
	return stream, nil
}

const segmentSelect = `SELECT p.seq,p.track,p.source,p.source_epoch,p.source_ms,p.offset_ms,p.media_ms,p.duration_ms,p.gap_ms,
	p.published_at,b.blob_key,b.digest,b.bytes FROM segments p JOIN blobs b ON b.blob_key=p.blob_key AND b.state='ready'`

type scanner interface {
	Scan(...any) error
}

func scanSegment(row scanner) (Segment, error) {
	var result Segment
	var digest []byte
	err := row.Scan(&result.Seq, &result.Track, &result.Source, &result.SourceEpoch, &result.SourceMS, &result.OffsetMS,
		&result.MediaMS, &result.DurationMS, &result.GapMS, &result.Published, &result.Blob.Key, &digest, &result.Blob.Bytes)
	copy(result.Blob.Digest[:], digest)
	return result, err
}

func (s *Store) Segments(ctx context.Context, id string, first, until uint64) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx, segmentSelect+" WHERE p.session_id=? AND p.seq>=? AND p.seq<? ORDER BY p.seq,p.track", id, first, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	segments := make([]Segment, 0)
	for rows.Next() {
		value, err := scanSegment(rows)
		if err != nil {
			return nil, err
		}
		segments = append(segments, value)
	}
	return segments, rows.Err()
}

func (s *Store) ReadSegment(ctx context.Context, id, track string, seq uint64) (Segment, error) {
	value, err := scanSegment(s.db.QueryRowContext(ctx, segmentSelect+" WHERE p.session_id=? AND p.track=? AND p.seq=?", id, track, seq))
	return value, missing(err)
}

// PriorDiscontinuities keeps the epoch number stable as a live window slides.
func (s *Store) PriorDiscontinuities(ctx context.Context, id string, until uint64) (uint64, error) {
	var count uint64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT seq, MIN(source_epoch) AS epoch,MIN(gap_ms) AS gap_ms FROM segments
		WHERE session_id=? AND seq<? GROUP BY seq
	) AS indexed
	LEFT JOIN (
		SELECT seq,MIN(source_epoch) AS epoch FROM segments WHERE session_id=? AND seq<? GROUP BY seq
	) AS previous ON previous.seq+1=indexed.seq
	WHERE indexed.gap_ms>0 OR (previous.epoch IS NOT NULL AND previous.epoch<>indexed.epoch)`, id, until, id, until).Scan(&count)
	return count, err
}

func (s *Store) LocalDiscontinuity(ctx context.Context, id string, seq uint64) (bool, error) {
	var gap int64
	var epoch, prior uint64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(gap_ms),0),COALESCE(MAX(source_epoch),0),
		(SELECT COALESCE(MAX(source_epoch),0) FROM segments WHERE session_id=? AND seq+1=?)
		FROM segments WHERE session_id=? AND seq=?`, id, seq, id, seq).Scan(&gap, &epoch, &prior)
	return gap > 0 || (prior != 0 && prior != epoch), err
}

func (s *Store) SetVisibility(ctx context.Context, caller Identity, id string, revision uint64, until *time.Time, hide bool) (Recording, error) {
	if (caller.Role != "editor" && caller.Role != "owner") || revision < 1 {
		return Recording{}, ErrDenied
	}
	if until != nil && (!until.After(time.Now()) || until.Year() > 2099) {
		return Recording{}, ErrInvalid
	}
	return transaction(ctx, s, func(tx *sql.Tx) (Recording, error) {
		rec, err := readRecording(ctx, tx, id, caller, false, true)
		if err != nil {
			return Recording{}, err
		}
		if rec.Revision != revision || rec.State != "published" {
			return Recording{}, ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recordings SET
			state=IF(?,'hidden','published'),hidden_at=IF(?,UTC_TIMESTAMP(6),NULL),visible_until=?,revision=revision+1 WHERE id=?`,
			hide, hide, until, id); err != nil {
			return Recording{}, err
		}
		if err := audit(ctx, tx, rec.Session, "recording_visibility", caller.Label, map[string]any{"hidden": hide, "visible_until": until}); err != nil {
			return Recording{}, err
		}
		return readRecording(ctx, tx, id, caller, false, false)
	})
}

type Event struct {
	ID      uint64          `json:"id"`
	Kind    string          `json:"kind"`
	Actor   string          `json:"actor"`
	Details json.RawMessage `json:"details"`
	At      time.Time       `json:"at"`
}

func (s *Store) Events(ctx context.Context, caller Identity, id string, after uint64) ([]Event, error) {
	stream, err := s.Session(ctx, caller, id)
	if err != nil {
		return nil, err
	}
	if !caller.Allows(stream.Tenant, stream.Channel, id, "owner", "editor", "director", "operator") {
		return nil, ErrDenied
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,kind,actor,details,created_at FROM session_events WHERE session_id=? AND id>? ORDER BY id LIMIT 200",
		id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var value Event
		if err := rows.Scan(&value.ID, &value.Kind, &value.Actor, &value.Details, &value.At); err != nil {
			return nil, err
		}
		events = append(events, value)
	}
	return events, rows.Err()
}

type QoE struct {
	Device    string `json:"device"`
	Key       string `json:"event_key"`
	Kind      string `json:"kind"`
	Duration  int64  `json:"duration_ms"`
	MediaTime int64  `json:"media_ms"`
}

func (s *Store) RecordQoE(ctx context.Context, caller Identity, id string, input QoE) (bool, error) {
	if !identifier.MatchString(input.Device) || !requestKey.MatchString(input.Key) || input.Duration < 0 ||
		input.Duration > 3_600_000 || input.MediaTime < 0 || input.MediaTime > 172_800_000 {
		return false, ErrInvalid
	}
	switch input.Kind {
	case "startup", "stall", "ended", "error", "heartbeat":
	default:
		return false, ErrInvalid
	}
	if _, err := s.Playback(ctx, caller, id, false); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO qoe_events(session_id,device,event_key,kind,duration_ms,media_ms)
		VALUES(?,?,?,?,?,?)`, id, input.Device, input.Key, input.Kind, input.Duration, input.MediaTime)
	if duplicate(err) {
		var current QoE
		err := s.db.QueryRowContext(ctx, "SELECT device,event_key,kind,duration_ms,media_ms FROM qoe_events WHERE session_id=? AND device=? AND event_key=?",
			id, input.Device, input.Key).Scan(&current.Device, &current.Key, &current.Kind, &current.Duration, &current.MediaTime)
		if err == nil && current != input {
			err = ErrConflict
		}
		return true, err
	}
	if err != nil {
		return false, err
	}
	return false, fencedResult(result, nil)
}

type Measurements struct {
	Session      string `json:"session_id"`
	Starts       uint64 `json:"starts"`
	StartupMS    uint64 `json:"startup_ms"`
	Stalls       uint64 `json:"stalls"`
	StalledMS    uint64 `json:"stalled_ms"`
	PlayerErrors uint64 `json:"player_errors"`
	IngressBytes uint64 `json:"ingress_bytes"`
	EgressBytes  uint64 `json:"egress_bytes"`
	Requests     uint64 `json:"media_requests"`
	EgressErrors uint64 `json:"egress_errors"`
	Switches     uint64 `json:"switches"`
	WaitingJobs  uint64 `json:"waiting_jobs"`
	FailedJobs   uint64 `json:"failed_jobs"`
}

func (s *Store) Measure(ctx context.Context, caller Identity, id string) (Measurements, error) {
	stream, err := s.Session(ctx, caller, id)
	if err != nil {
		return Measurements{}, err
	}
	if !caller.Allows(stream.Tenant, stream.Channel, id, "owner", "director", "editor", "operator") {
		return Measurements{}, ErrDenied
	}
	value := Measurements{Session: id}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(CASE WHEN kind='startup' THEN 1 END),
		COALESCE(SUM(CASE WHEN kind='startup' THEN duration_ms END),0),COUNT(CASE WHEN kind='stall' THEN 1 END),
		COALESCE(SUM(CASE WHEN kind='stall' THEN duration_ms END),0),COUNT(CASE WHEN kind='error' THEN 1 END)
		FROM qoe_events WHERE session_id=?`, id).Scan(&value.Starts, &value.StartupMS, &value.Stalls, &value.StalledMS, &value.PlayerErrors); err != nil {
		return Measurements{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(ingress_bytes),0),COALESCE(SUM(egress_bytes),0),
		COALESCE(SUM(media_requests),0),COALESCE(SUM(egress_errors),0) FROM usage_hourly WHERE session_id=?`, id).
		Scan(&value.IngressBytes, &value.EgressBytes, &value.Requests, &value.EgressErrors); err != nil {
		return Measurements{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(CASE WHEN state IN ('queued','running') THEN 1 END),
		COUNT(CASE WHEN state='failed' THEN 1 END) FROM transcode_jobs WHERE session_id=?`, id).Scan(&value.WaitingJobs, &value.FailedJobs); err != nil {
		return Measurements{}, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_events WHERE session_id=? AND kind='input_selected'
		AND JSON_UNQUOTE(JSON_EXTRACT(details,'$.previous'))<>''`, id).Scan(&value.Switches)
	return value, err
}

type Usage struct {
	Hour         time.Time `json:"hour"`
	IngressBytes uint64    `json:"ingress_bytes"`
	EgressBytes  uint64    `json:"egress_bytes"`
	Requests     uint64    `json:"media_requests"`
	EgressErrors uint64    `json:"egress_errors"`
}

func (s *Store) Usage(ctx context.Context, caller Identity, since time.Time) ([]Usage, error) {
	if caller.Role != "operator" && caller.Role != "owner" {
		return nil, ErrDenied
	}
	if since.IsZero() {
		since = time.Now().AddDate(0, 0, -7)
	}
	if since.Before(time.Now().AddDate(0, 0, -90)) || since.After(time.Now()) {
		return nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.hour_start,SUM(u.ingress_bytes),SUM(u.egress_bytes),SUM(u.media_requests),SUM(u.egress_errors)
		FROM usage_hourly u JOIN sessions s ON s.id=u.session_id WHERE s.tenant=? AND u.hour_start>=?
		AND (?='' OR s.channel=?) GROUP BY u.hour_start ORDER BY u.hour_start DESC`, caller.Tenant, since.Truncate(time.Hour), caller.Channel, caller.Channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]Usage, 0)
	for rows.Next() {
		var value Usage
		if err := rows.Scan(&value.Hour, &value.IngressBytes, &value.EgressBytes, &value.Requests, &value.EgressErrors); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
