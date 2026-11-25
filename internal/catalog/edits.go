package catalog

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"
)

var languageTag = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`)

type Caption struct {
	ID       string `json:"id"`
	Language string `json:"language"`
	Revision uint64 `json:"revision"`
	Blob     Blob   `json:"-"`
}

func (s *Store) AddCaption(ctx context.Context, caller Identity, recording, language, idempotency string, blob Blob) (Caption, error) {
	if caller.Role != "owner" && caller.Role != "editor" {
		return Caption{}, ErrDenied
	}
	if !languageTag.MatchString(language) || !requestKey.MatchString(idempotency) || blob.Bytes > 1<<20 {
		return Caption{}, ErrInvalid
	}
	fingerprint := digest(struct {
		Recording, Language string
		Digest              [32]byte
	}{recording, language, blob.Digest})
	return transaction(ctx, s, func(tx *sql.Tx) (Caption, error) {
		rec, err := readRecording(ctx, tx, recording, caller, false, true)
		if err != nil {
			return Caption{}, err
		}
		id, found, err := receipt(ctx, tx, caller.Tenant, "caption", idempotency, fingerprint)
		if err != nil {
			return Caption{}, err
		}
		if found {
			return readCaption(ctx, tx, recording, id)
		}
		if rec.State != "published" {
			return Caption{}, ErrConflict
		}
		if err := registerBlob(ctx, tx, blob); err != nil {
			return Caption{}, err
		}
		result := Caption{ID: key(), Language: language, Blob: blob}
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision),0)+1 FROM captions WHERE recording_id=? AND language=?",
			recording, language).Scan(&result.Revision); err != nil {
			return Caption{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO captions(id,recording_id,language,revision,blob_key) VALUES(?,?,?,?,?)",
			result.ID, recording, language, result.Revision, blob.Key); err != nil {
			return Caption{}, err
		}
		if err := saveReceipt(ctx, tx, caller.Tenant, "caption", idempotency, fingerprint, result.ID); err != nil {
			return Caption{}, err
		}
		return result, audit(ctx, tx, rec.Session, "caption_added", caller.Label, result)
	})
}

func readCaption(ctx context.Context, db queryer, recording, id string) (Caption, error) {
	var value Caption
	var digest []byte
	err := db.QueryRowContext(ctx, `SELECT c.id,c.language,c.revision,b.blob_key,b.digest,b.bytes FROM captions c
		JOIN blobs b ON b.blob_key=c.blob_key AND b.state='ready' WHERE c.recording_id=? AND c.id=?`, recording, id).
		Scan(&value.ID, &value.Language, &value.Revision, &value.Blob.Key, &digest, &value.Blob.Bytes)
	copy(value.Blob.Digest[:], digest)
	return value, missing(err)
}

func (s *Store) Caption(ctx context.Context, caller Identity, recording, id string) (Caption, error) {
	if _, err := s.Playback(ctx, caller, recording, true); err != nil {
		return Caption{}, err
	}
	return readCaption(ctx, s.db, recording, id)
}

func (s *Store) Captions(ctx context.Context, caller Identity, id string) ([]Caption, error) {
	if _, err := s.Playback(ctx, caller, id, true); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.language,c.revision FROM captions c
		JOIN blobs b ON c.blob_key=b.blob_key AND b.state='ready' WHERE recording_id=?
		AND NOT EXISTS(SELECT 1 FROM captions n WHERE n.recording_id=c.recording_id AND n.language=c.language AND n.revision>c.revision)
		ORDER BY c.language LIMIT 64`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]Caption, 0)
	for rows.Next() {
		var value Caption
		if err := rows.Scan(&value.ID, &value.Language, &value.Revision); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

type EditRequest struct {
	Revision     uint64     `json:"recording_revision"`
	FromMS       int64      `json:"from_ms"`
	ToMS         int64      `json:"to_ms"`
	Caption      string     `json:"caption_id,omitempty"`
	VisibleUntil *time.Time `json:"visible_until,omitempty"`
}

type Edit struct {
	ID          string     `json:"id"`
	Recording   string     `json:"recording_id"`
	Session     string     `json:"session_id"`
	Channel     string     `json:"channel"`
	State       string     `json:"state"`
	Revision    uint64     `json:"recording_revision"`
	Caption     string     `json:"caption_id,omitempty"`
	FromMS      int64      `json:"from_ms"`
	ToMS        int64      `json:"to_ms"`
	ActualFrom  *int64     `json:"actual_from_ms,omitempty"`
	ActualTo    *int64     `json:"actual_to_ms,omitempty"`
	Created     time.Time  `json:"created_at"`
	Until       *time.Time `json:"visible_until,omitempty"`
	Error       string     `json:"error,omitempty"`
	HasCaptions bool       `json:"has_captions"`
}

const editSelect = `SELECT e.id,e.recording_id,r.session_id,s.channel,e.state,e.recording_revision,COALESCE(e.caption_id,''),
	e.from_ms,e.to_ms,e.actual_from_ms,e.actual_to_ms,e.created_at,e.visible_until,COALESCE(e.error,''),e.caption_blob IS NOT NULL
	FROM edits e JOIN recordings r ON r.id=e.recording_id JOIN sessions s ON s.id=r.session_id`

func scanEdit(row scanner) (Edit, error) {
	var edit Edit
	err := row.Scan(&edit.ID, &edit.Recording, &edit.Session, &edit.Channel, &edit.State, &edit.Revision, &edit.Caption,
		&edit.FromMS, &edit.ToMS, &edit.ActualFrom, &edit.ActualTo, &edit.Created, &edit.Until, &edit.Error, &edit.HasCaptions)
	return edit, missing(err)
}

func readEdit(ctx context.Context, db queryer, caller Identity, id string, published bool) (Edit, error) {
	query := editSelect + " WHERE s.tenant=? AND e.id=? AND (?='' OR s.channel=?) AND (?='' OR s.id=?)"
	if published || caller.Role == "viewer" {
		query += " AND e.state='published' AND (e.visible_until IS NULL OR e.visible_until>UTC_TIMESTAMP(6))"
	}
	return scanEdit(db.QueryRowContext(ctx, query, caller.Tenant, id, caller.Channel, caller.Channel, caller.Session, caller.Session))
}

func (s *Store) CreateEdit(ctx context.Context, caller Identity, id, idempotency string, request EditRequest) (Edit, error) {
	if caller.Role != "owner" && caller.Role != "editor" {
		return Edit{}, ErrDenied
	}
	if !requestKey.MatchString(idempotency) || request.Revision < 1 || request.FromMS < 0 || request.ToMS <= request.FromMS ||
		request.ToMS > 172_800_000 || request.ToMS-request.FromMS > 1_800_000 ||
		(request.VisibleUntil != nil && (!request.VisibleUntil.After(time.Now()) || request.VisibleUntil.Year() > 2099)) {
		return Edit{}, ErrInvalid
	}
	if request.VisibleUntil != nil {
		until := request.VisibleUntil.UTC()
		request.VisibleUntil = &until
	}
	fingerprint := digest(struct {
		Recording string
		EditRequest
	}{id, request})
	return transaction(ctx, s, func(tx *sql.Tx) (Edit, error) {
		rec, err := readRecording(ctx, tx, id, caller, false, true)
		if err != nil {
			return Edit{}, err
		}
		edit, found, err := receipt(ctx, tx, caller.Tenant, "edit", idempotency, fingerprint)
		if err != nil {
			return Edit{}, err
		}
		if found {
			return readEdit(ctx, tx, caller, edit, false)
		}
		if rec.State != "published" || rec.Revision != request.Revision || request.ToMS > rec.DurationMS {
			return Edit{}, ErrConflict
		}
		if request.Caption != "" {
			if _, err := readCaption(ctx, tx, id, request.Caption); err != nil {
				return Edit{}, err
			}
		}
		edit = key()
		if _, err := tx.ExecContext(ctx, `INSERT INTO edits
			(id,recording_id,recording_revision,caption_id,from_ms,to_ms,state,visible_until)
			VALUES(?,?,?,NULLIF(?,''),?,?,'queued',?)`, edit, rec.ID, rec.Revision, request.Caption, request.FromMS, request.ToMS,
			request.VisibleUntil); err != nil {
			return Edit{}, err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO edit_segments(edit_id,seq,track,blob_key,media_ms,duration_ms)
			SELECT ?,p.seq,p.track,p.blob_key,p.media_ms,p.duration_ms
			FROM segments p JOIN blobs b ON b.blob_key=p.blob_key AND b.state='ready'
			WHERE p.session_id=? AND p.media_ms<? AND p.media_ms+p.duration_ms>?`, edit, id, request.ToMS, request.FromMS)
		if err != nil {
			return Edit{}, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return Edit{}, err
		}
		if count == 0 {
			return Edit{}, ErrConflict
		}
		if err := checkEditCoverage(ctx, tx, edit, request.FromMS, request.ToMS); err != nil {
			return Edit{}, err
		}
		if err := saveReceipt(ctx, tx, caller.Tenant, "edit", idempotency, fingerprint, edit); err != nil {
			return Edit{}, err
		}
		if err := audit(ctx, tx, rec.Session, "edit_queued", caller.Label, map[string]any{"edit_id": edit, "range": request}); err != nil {
			return Edit{}, err
		}
		return readEdit(ctx, tx, caller, edit, false)
	})
}

func checkEditCoverage(ctx context.Context, tx *sql.Tx, edit string, start, end int64) error {
	var lower, upper int64
	var gaps int
	err := tx.QueryRowContext(ctx, `SELECT MIN(media_ms),MAX(media_ms+duration_ms),
		COUNT(CASE WHEN previous IS NOT NULL AND previous<>media_ms THEN 1 END) FROM (
			SELECT media_ms,duration_ms,LAG(media_ms+duration_ms) OVER(ORDER BY seq) AS previous FROM (
				SELECT seq,MIN(media_ms) AS media_ms,MIN(duration_ms) AS duration_ms FROM edit_segments WHERE edit_id=? GROUP BY seq
			) AS intervals
		) bounded`, edit).Scan(&lower, &upper, &gaps)
	if err == nil && (gaps != 0 || lower > start || upper < end) {
		err = ErrConflict
	}
	return err
}

func (s *Store) Edit(ctx context.Context, caller Identity, id string) (Edit, error) {
	if caller.Role == "push" {
		return Edit{}, ErrDenied
	}
	return readEdit(ctx, s.db, caller, id, false)
}

func (s *Store) Edits(ctx context.Context, caller Identity, recording, after string) ([]Edit, error) {
	if caller.Role == "push" {
		return nil, ErrDenied
	}
	if _, err := readRecording(ctx, s.db, recording, caller, false, false); err != nil {
		return nil, err
	}
	query := editSelect + " WHERE e.recording_id=? AND e.id>?"
	if caller.Role == "viewer" {
		query += " AND e.state='published' AND (e.visible_until IS NULL OR e.visible_until>UTC_TIMESTAMP(6))"
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY e.id LIMIT 101", recording, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]Edit, 0)
	for rows.Next() {
		edit, err := scanEdit(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, edit)
	}
	return values, rows.Err()
}

func (s *Store) EditMedia(ctx context.Context, caller Identity, id string, captions bool) (Edit, Blob, error) {
	edit, err := readEdit(ctx, s.db, caller, id, true)
	if err != nil {
		return Edit{}, Blob{}, err
	}
	var blob Blob
	var digest []byte
	selection := "e.output_blob"
	if captions {
		selection = "e.caption_blob"
	}
	err = s.db.QueryRowContext(ctx, `SELECT b.blob_key,b.digest,b.bytes FROM edits e
		JOIN blobs b ON b.blob_key=`+selection+` AND b.state='ready'
		WHERE e.id=? AND e.state='published' AND (e.visible_until IS NULL OR e.visible_until>UTC_TIMESTAMP(6))`, id).
		Scan(&blob.Key, &digest, &blob.Bytes)
	copy(blob.Digest[:], digest)
	return edit, blob, missing(err)
}

type ClipFragment struct {
	Blobs      []Blob
	MediaStart int64
	Duration   int
}

type EditJob struct {
	ID        string
	Session   string
	Recording string
	Node      string
	Attempt   int
	Start     int64
	End       int64
	Caption   string
}

func (s *Store) ClaimEdit(ctx context.Context, node string) (*EditJob, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (*EditJob, error) {
		capacity, err := nodeCapacity(ctx, tx, node)
		if err != nil || !capacity {
			return nil, err
		}
		var job EditJob
		err = tx.QueryRowContext(ctx, `SELECT id,recording_id,attempts,from_ms,to_ms,COALESCE(caption_id,'') FROM edits
			WHERE state='queued' OR (state='running' AND lease_until<=UTC_TIMESTAMP(6))
			ORDER BY created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&job.ID, &job.Recording, &job.Attempt, &job.Start, &job.End, &job.Caption)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if job.Attempt >= maxAttempts {
			_, err := tx.ExecContext(ctx, "UPDATE edits SET state='failed',error='lease attempts exhausted' WHERE id=?", job.ID)
			return nil, err
		}
		job.Attempt++
		job.Node = node
		if err := tx.QueryRowContext(ctx, "SELECT session_id FROM recordings WHERE id=?", job.Recording).Scan(&job.Session); err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE edits SET state='running',attempts=?,node=?,error=NULL,
			lease_until=TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)) WHERE id=?`, job.Attempt, node, leaseSeconds, job.ID)
		return &job, err
	})
}

func (s *Store) RenewEdit(ctx context.Context, job EditJob) error {
	result, err := s.db.ExecContext(ctx, `UPDATE edits SET lease_until=TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6))
		WHERE id=? AND node=? AND attempts=? AND state='running' AND lease_until>UTC_TIMESTAMP(6)`,
		leaseSeconds, job.ID, job.Node, job.Attempt)
	return fencedResult(result, err)
}

func (s *Store) EditInputs(ctx context.Context, job EditJob) ([]ClipFragment, *Caption, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.media_ms,p.duration_ms,b.blob_key,b.digest,b.bytes
		FROM edit_segments p JOIN blobs b ON b.blob_key=p.blob_key AND b.state='ready' WHERE p.edit_id=?
		ORDER BY p.seq,FIELD(p.track,'240p','360p','720p') DESC`, job.ID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var values []ClipFragment
	for rows.Next() {
		var value ClipFragment
		var blob Blob
		var digest []byte
		if err := rows.Scan(&value.MediaStart, &value.Duration, &blob.Key, &digest, &blob.Bytes); err != nil {
			return nil, nil, err
		}
		copy(blob.Digest[:], digest)
		last := len(values) - 1
		if last >= 0 && values[last].MediaStart == value.MediaStart {
			values[last].Blobs = append(values[last].Blobs, blob)
		} else {
			value.Blobs = []Blob{blob}
			values = append(values, value)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(values) == 0 {
		return nil, nil, ErrMissing
	}
	if job.Caption == "" {
		return values, nil, nil
	}
	caption, err := readCaption(ctx, s.db, job.Recording, job.Caption)
	return values, &caption, err
}

func (s *Store) CompleteEdit(ctx context.Context, job EditJob, output, captions Blob, from, to int64, failure string) error {
	success := output.Key != "" && to > from
	_, err := transaction(ctx, s, func(tx *sql.Tx) (struct{}, error) {
		var zero struct{}
		state := "queued"
		if job.Attempt >= maxAttempts {
			state = "failed"
		}
		var actualFrom, actualTo *int64
		if success {
			state, actualFrom, actualTo = "published", &from, &to
			if err := registerBlob(ctx, tx, output); err != nil {
				return zero, err
			}
			if captions.Key != "" {
				if err := registerBlob(ctx, tx, captions); err != nil {
					return zero, err
				}
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE edits SET state=?,output_blob=NULLIF(?,''),caption_blob=NULLIF(?,''),actual_from_ms=?,
			actual_to_ms=?,error=NULLIF(?,''),lease_until=NULL WHERE id=? AND node=? AND attempts=? AND state='running'
			AND lease_until>UTC_TIMESTAMP(6)`, state, output.Key, captions.Key, actualFrom, actualTo, shortDiagnostic(failure), job.ID, job.Node, job.Attempt)
		if err := fencedResult(result, err); err != nil {
			return zero, err
		}
		return zero, audit(ctx, tx, job.Session, "edit_result", job.Node, map[string]any{"id": job.ID, "state": state,
			"from_ms": actualFrom, "to_ms": actualTo, "attempt": job.Attempt})
	})
	return err
}
