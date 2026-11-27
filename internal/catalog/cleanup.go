package catalog

import (
	"context"
	"database/sql"
	"time"
)

func (s *Store) ExpirePublications(ctx context.Context) error {
	_, err := transaction(ctx, s, func(tx *sql.Tx) (struct{}, error) {
		var zero struct{}
		rows, err := tx.QueryContext(ctx, `SELECT id,session_id FROM recordings WHERE state='published'
			AND visible_until<=UTC_TIMESTAMP(6) ORDER BY visible_until,id LIMIT 64 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return zero, err
		}
		type recording struct{ id, session string }
		var expired []recording
		for rows.Next() {
			var rec recording
			if err := rows.Scan(&rec.id, &rec.session); err != nil {
				rows.Close()
				return zero, err
			}
			expired = append(expired, rec)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return zero, err
		}
		for _, rec := range expired {
			if _, err := tx.ExecContext(ctx, `UPDATE recordings SET state='hidden',hidden_at=UTC_TIMESTAMP(6),revision=revision+1 WHERE id=?`, rec.id); err != nil {
				return zero, err
			}
			if err := audit(ctx, tx, rec.session, "recording_expired", "maintenance", map[string]string{"recording_id": rec.id}); err != nil {
				return zero, err
			}
		}
		_, err = tx.ExecContext(ctx, "UPDATE edits SET state='hidden' WHERE state='published' AND visible_until<=UTC_TIMESTAMP(6) LIMIT 64")
		return zero, err
	})
	return err
}

func (s *Store) PurgeHidden(ctx context.Context, grace time.Duration) (int, error) {
	if grace < time.Minute {
		return 0, ErrInvalid
	}
	return transaction(ctx, s, func(tx *sql.Tx) (int, error) {
		var count int
		rows, err := tx.QueryContext(ctx, `SELECT r.id,r.session_id FROM recordings r
			WHERE r.state='hidden' AND r.hidden_at<TIMESTAMPADD(MICROSECOND,-?,UTC_TIMESTAMP(6))
			AND NOT EXISTS(SELECT 1 FROM edits e WHERE e.recording_id=r.id AND e.state IN ('queued','running'))
			ORDER BY r.hidden_at,r.id LIMIT 16 FOR UPDATE OF r SKIP LOCKED`, grace.Microseconds())
		if err != nil {
			return 0, err
		}
		type recording struct{ id, session string }
		var purge []recording
		for rows.Next() {
			var rec recording
			if err := rows.Scan(&rec.id, &rec.session); err != nil {
				rows.Close()
				return 0, err
			}
			purge = append(purge, rec)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, err
		}
		for _, rec := range purge {
			if _, err := tx.ExecContext(ctx, "DELETE FROM segments WHERE session_id=?", rec.session); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE recordings SET state='purged',revision=revision+1 WHERE id=?", rec.id); err != nil {
				return 0, err
			}
			if err := audit(ctx, tx, rec.session, "recording_purged", "maintenance", map[string]string{"recording_id": rec.id}); err != nil {
				return 0, err
			}
			count++
		}
		if _, err := tx.ExecContext(ctx, `UPDATE edits SET state='purged' WHERE state='hidden'
			AND visible_until<TIMESTAMPADD(MICROSECOND,-?,UTC_TIMESTAMP(6)) LIMIT 64`, grace.Microseconds()); err != nil {
			return 0, err
		}
		return count, nil
	})
}

// Collectible marks retired files before removal. Catalog rows retain checksums
// and source lineage even after the underlying files are reclaimed.
func (s *Store) Collectible(ctx context.Context, grace time.Duration) ([]string, error) {
	if grace < time.Minute {
		return nil, ErrInvalid
	}
	return transaction(ctx, s, func(tx *sql.Tx) ([]string, error) {
		rows, err := tx.QueryContext(ctx, `SELECT b.blob_key FROM blobs b WHERE b.state='deleting' OR (b.state='ready'
			AND b.created_at<TIMESTAMPADD(MICROSECOND,-?,UTC_TIMESTAMP(6))
			AND NOT EXISTS(SELECT 1 FROM segments p WHERE p.blob_key=b.blob_key)
			AND NOT EXISTS(SELECT 1 FROM input_fragments i JOIN sessions s ON s.id=i.session_id
				JOIN source_inputs t ON t.session_id=i.session_id AND t.source=i.source
				WHERE i.blob_key=b.blob_key AND s.state<>'archived' AND s.next_offset_ms<=i.offset_ms AND i.generation=t.generation)
			AND NOT EXISTS(SELECT 1 FROM transcode_jobs j WHERE j.input_blob=b.blob_key AND j.state IN ('queued','running'))
			AND NOT EXISTS(SELECT 1 FROM production_inputs i JOIN productions p ON p.id=i.production_id
				WHERE i.blob_key=b.blob_key AND p.state<>'sealed')
			AND NOT EXISTS(SELECT 1 FROM edit_segments f JOIN edits e ON e.id=f.edit_id
				WHERE f.blob_key=b.blob_key AND e.state IN ('queued','running'))
			AND NOT EXISTS(SELECT 1 FROM captions c JOIN recordings r ON r.id=c.recording_id
				WHERE c.blob_key=b.blob_key AND (r.state<>'purged' OR EXISTS(
					SELECT 1 FROM edits e WHERE e.caption_id=c.id AND e.state IN ('queued','running'))))
			AND NOT EXISTS(SELECT 1 FROM edits e WHERE (e.output_blob=b.blob_key OR e.caption_blob=b.blob_key) AND e.state<>'purged'))
			ORDER BY b.state DESC,b.created_at,b.blob_key LIMIT 128 FOR UPDATE OF b SKIP LOCKED`, grace.Microseconds())
		if err != nil {
			return nil, err
		}
		var values []string
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return nil, err
			}
			values = append(values, key)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, key := range values {
			if _, err := tx.ExecContext(ctx, "UPDATE blobs SET state='deleting' WHERE blob_key=?", key); err != nil {
				return nil, err
			}
		}
		return values, nil
	})
}

func (s *Store) MarkDeleted(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE blobs SET state='deleted' WHERE blob_key=? AND state='deleting'", key)
	return err
}

func (s *Store) KnownBlob(ctx context.Context, key string) (bool, error) {
	var present bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM blobs WHERE blob_key=?)", key).Scan(&present)
	return present, err
}
