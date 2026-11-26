package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"
)

type VisibilityTarget struct {
	ID       string `json:"recording_id"`
	Revision uint64 `json:"expected_revision"`
}

type VisibilityBatchRequest struct {
	Targets []VisibilityTarget `json:"items"`
	Hide    bool               `json:"hide"`
	Until   *time.Time         `json:"visible_until,omitempty"`
}

type VisibilityBatchSummary struct {
	ID        uint64     `json:"id"`
	Key       string     `json:"request_key"`
	Actor     string     `json:"actor"`
	Hide      bool       `json:"hide"`
	Until     *time.Time `json:"visible_until,omitempty"`
	Count     int        `json:"item_count"`
	Processed int        `json:"processed"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
}

type VisibilityBatch struct {
	VisibilityBatchSummary
	Items []VisibilityBatchItem `json:"items"`
}

type VisibilityBatchItem struct {
	VisibilityTarget
	State           string  `json:"state"`
	AppliedRevision *uint64 `json:"applied_revision,omitempty"`
	Reason          *string `json:"reason,omitempty"`
}

const visibilityBatchSelect = `SELECT id,request_key,actor,hide,visible_until,item_count,processed,created_at
	FROM visibility_batches WHERE tenant=? AND credential_digest=?`

func readVisibilityBatch(row scanner) (VisibilityBatchSummary, error) {
	var value VisibilityBatchSummary
	err := row.Scan(&value.ID, &value.Key, &value.Actor, &value.Hide, &value.Until, &value.Count, &value.Processed, &value.CreatedAt)
	switch {
	case value.Processed == 0:
		value.State = "prepared"
	case value.Processed == value.Count:
		value.State = "completed"
	default:
		value.State = "running"
	}
	return value, missing(err)
}

func (i Identity) mayManageVisibilityBatches() bool {
	return (i.Role == "owner" || i.Role == "editor") && i.credential != [32]byte{}
}

func (s *Store) PrepareVisibilityBatch(ctx context.Context, caller Identity, request string, input VisibilityBatchRequest) (VisibilityBatch, error) {
	if !caller.mayManageVisibilityBatches() {
		return VisibilityBatch{}, ErrDenied
	}
	if !requestKey.MatchString(request) || len(input.Targets) == 0 || len(input.Targets) > 50 ||
		input.Hide != (input.Until == nil) {
		return VisibilityBatch{}, ErrInvalid
	}
	if input.Until != nil {
		until := input.Until.UTC().Truncate(time.Microsecond)
		if until.Year() < 2000 || until.Year() > 2099 {
			return VisibilityBatch{}, ErrInvalid
		}
		input.Until = &until
	}
	seen := make(map[string]bool, len(input.Targets))
	for _, item := range input.Targets {
		if len(item.ID) != 24 || !identifier.MatchString(item.ID) || item.Revision == 0 || seen[item.ID] {
			return VisibilityBatch{}, ErrInvalid
		}
		seen[item.ID] = true
	}
	checksum := digest(input)
	_, err := transaction(ctx, s, func(tx *sql.Tx) (struct{}, error) {
		var zero struct{}
		result, err := tx.ExecContext(ctx, `INSERT INTO visibility_batches
			(tenant,credential_digest,request_key,request_digest,actor,hide,visible_until,item_count)
			VALUES(?,?,?,?,?,?,?,?)`, caller.Tenant, caller.credential[:], request, checksum[:],
			caller.Label, input.Hide, input.Until, len(input.Targets))
		if duplicate(err) {
			var stored []byte
			err = tx.QueryRowContext(ctx, `SELECT request_digest FROM visibility_batches
				WHERE tenant=? AND credential_digest=? AND request_key=?`,
				caller.Tenant, caller.credential[:], request).Scan(&stored)
			if err == nil && !bytes.Equal(stored, checksum[:]) {
				err = ErrConflict
			}
			return zero, err
		}
		if err != nil {
			return zero, err
		}
		if input.Until != nil && !input.Until.After(time.Now()) {
			return zero, ErrInvalid
		}
		id, err := result.LastInsertId()
		if err != nil {
			return zero, err
		}
		for position, item := range input.Targets {
			if _, err := tx.ExecContext(ctx, `INSERT INTO visibility_batch_items
				(batch_id,position,recording_id,expected_revision) VALUES(?,?,?,?)`, id, position, item.ID, item.Revision); err != nil {
				return zero, err
			}
		}
		return zero, nil
	})
	if err != nil {
		return VisibilityBatch{}, err
	}
	return s.VisibilityBatch(ctx, caller, request)
}

func (s *Store) VisibilityBatches(ctx context.Context, caller Identity, before uint64) ([]VisibilityBatchSummary, error) {
	if !caller.mayManageVisibilityBatches() {
		return nil, ErrDenied
	}
	rows, err := s.db.QueryContext(ctx, visibilityBatchSelect+" AND (?=0 OR id<?) ORDER BY id DESC LIMIT 26",
		caller.Tenant, caller.credential[:], before, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]VisibilityBatchSummary, 0)
	for rows.Next() {
		value, err := readVisibilityBatch(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) VisibilityBatch(ctx context.Context, caller Identity, request string) (VisibilityBatch, error) {
	if !caller.mayManageVisibilityBatches() {
		return VisibilityBatch{}, ErrDenied
	}
	if !requestKey.MatchString(request) {
		return VisibilityBatch{}, ErrInvalid
	}
	return transaction(ctx, s, func(tx *sql.Tx) (VisibilityBatch, error) {
		value, err := readVisibilityBatch(tx.QueryRowContext(ctx, visibilityBatchSelect+" AND request_key=? FOR SHARE",
			caller.Tenant, caller.credential[:], request))
		if err != nil {
			return VisibilityBatch{}, err
		}
		rows, err := tx.QueryContext(ctx, `SELECT recording_id,expected_revision,state,applied_revision,reason
			FROM visibility_batch_items WHERE batch_id=? ORDER BY position`, value.ID)
		if err != nil {
			return VisibilityBatch{}, err
		}
		defer rows.Close()
		batch := VisibilityBatch{VisibilityBatchSummary: value, Items: make([]VisibilityBatchItem, 0, value.Count)}
		for rows.Next() {
			var item VisibilityBatchItem
			if err := rows.Scan(&item.ID, &item.Revision, &item.State, &item.AppliedRevision, &item.Reason); err != nil {
				return VisibilityBatch{}, err
			}
			batch.Items = append(batch.Items, item)
		}
		return batch, rows.Err()
	})
}

func (s *Store) ExecuteVisibilityBatch(ctx context.Context, caller Identity, request string) (VisibilityBatch, error) {
	if !caller.mayManageVisibilityBatches() {
		return VisibilityBatch{}, ErrDenied
	}
	if !requestKey.MatchString(request) {
		return VisibilityBatch{}, ErrInvalid
	}
	// One recording and its receipt commit together. A lost caller can continue
	// at the next persisted position, including after another caller takes over.
	for {
		done, err := transaction(ctx, s, func(tx *sql.Tx) (bool, error) {
			return executeVisibilityItem(ctx, tx, caller, request)
		})
		if err != nil {
			return VisibilityBatch{}, err
		}
		if done {
			return s.VisibilityBatch(ctx, caller, request)
		}
	}
}

func executeVisibilityItem(ctx context.Context, tx *sql.Tx, caller Identity, request string) (bool, error) {
	batch, err := readVisibilityBatch(tx.QueryRowContext(ctx, visibilityBatchSelect+" AND request_key=? FOR UPDATE",
		caller.Tenant, caller.credential[:], request))
	if err != nil || batch.Processed == batch.Count {
		return true, err
	}
	var item VisibilityTarget
	if err := tx.QueryRowContext(ctx, `SELECT recording_id,expected_revision FROM visibility_batch_items
		WHERE batch_id=? AND position=? AND state='not_started'`, batch.ID, batch.Processed).Scan(&item.ID, &item.Revision); err != nil {
		return false, err
	}
	var revision *uint64
	var reason string
	if batch.Until != nil && !batch.Until.After(time.Now()) {
		reason = "deadline_elapsed"
	} else {
		value, err := setVisibility(ctx, tx, caller, item.ID, item.Revision, batch.Until, batch.Hide, batch.Key)
		switch {
		case err == nil:
			revision = &value.Revision
		case errors.Is(err, ErrMissing):
			reason = "not_found"
		case errors.Is(err, ErrConflict):
			reason = "revision_or_state_changed"
		default:
			return false, err
		}
	}
	state := "applied"
	if reason != "" {
		state = "rejected"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE visibility_batch_items SET state=?,applied_revision=?,reason=NULLIF(?,'')
		WHERE batch_id=? AND position=?`, state, revision, reason, batch.ID, batch.Processed); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE visibility_batches SET processed=processed+1 WHERE id=?", batch.ID)
	return batch.Processed+1 == batch.Count, err
}
