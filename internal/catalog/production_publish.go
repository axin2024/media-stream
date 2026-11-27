package catalog

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const ProductionMuxStart = ProductionTimebase

type ProductionTrack struct {
	Track      string `json:"track"`
	VideoFirst int64  `json:"video_first_pts"`
	VideoLast  int64  `json:"video_last_pts"`
	AudioFirst int64  `json:"audio_first_pts"`
	AudioLast  int64  `json:"audio_last_pts"`
	VideoCount int    `json:"video_frames"`
	AudioCount int    `json:"audio_packets"`
	AudioRate  int    `json:"audio_sample_rate"`
	SHA256     string `json:"sha256"`
}

type RenderedTrack struct {
	Stats ProductionTrack
	Blob  Blob
}

func (s *Store) ReportProductionRenderFailure(ctx context.Context, job ProductionJob, reason string) error {
	if len(reason) > 255 {
		reason = "renderer did not produce usable media"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE production_frames SET last_error=?,lease_until=TIMESTAMPADD(SECOND,1,UTC_TIMESTAMP(6))
		WHERE production_id=? AND start_frame=? AND claim=? AND sequence_no IS NULL`, reason, job.Production, job.Start, job.Claim)
	return err
}

func (s *Store) PublishProductionFrames(ctx context.Context, job ProductionJob, output []RenderedTrack) error {
	_, err := transaction(ctx, s, func(tx *sql.Tx) (struct{}, error) {
		var zero struct{}
		p, err := readProduction(ctx, tx, job.Production, true)
		if err != nil {
			return zero, err
		}
		var claim string
		var lease time.Time
		var seq *uint64
		var size int
		if err := tx.QueryRowContext(ctx, `SELECT claim,lease_until,sequence_no,frame_count FROM production_frames
			WHERE production_id=? AND start_frame=?`, job.Production, job.Start).Scan(&claim, &lease, &seq, &size); err != nil {
			if err == sql.ErrNoRows {
				return zero, ErrFenced
			}
			return zero, err
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return zero, err
		}
		if claim != job.Claim || seq != nil || !lease.After(now) || p.Frame != job.Start || size != job.Frames ||
			(p.State != "on_air" && p.State != "ending") {
			return zero, ErrFenced
		}
		stream, err := readSession(ctx, tx, p.Output, true)
		if err != nil {
			return zero, err
		}
		if stream.MediaDurationMS != job.Start*ProductionFrameMS || stream.State == "archived" {
			return zero, ErrFenced
		}
		profiles := Profiles(stream.Preset)
		if len(output) != len(profiles) {
			return zero, ErrInvalid
		}
		wantFirst := ProductionMuxStart + job.Start*(ProductionTimebase/ProductionFPS)
		wantLast := wantFirst + int64(job.Frames-1)*(ProductionTimebase/ProductionFPS)
		previous := make(map[string]ProductionTrack, len(profiles))
		if stream.NextSeq > 0 {
			var encoded []byte
			if err := tx.QueryRowContext(ctx, `SELECT evidence FROM production_frames WHERE production_id=? AND sequence_no=?`,
				p.ID, stream.NextSeq-1).Scan(&encoded); err != nil {
				return zero, err
			}
			var tracks []ProductionTrack
			if err := json.Unmarshal(encoded, &tracks); err != nil {
				return zero, err
			}
			for _, track := range tracks {
				previous[track.Track] = track
			}
		}
		duration := job.Frames * ProductionFrameMS
		evidence := make([]ProductionTrack, 0, len(profiles))
		for index, out := range output {
			stats := out.Stats
			if stats.Track != profiles[index].Track || stats.VideoFirst != wantFirst || stats.VideoLast != wantLast ||
				stats.VideoCount != job.Frames || stats.AudioCount == 0 || stats.AudioRate != ProductionAudioRate {
				return zero, fmt.Errorf("production segment clock validation failed for %s", stats.Track)
			}
			if prior, found := previous[stats.Track]; found && (stats.VideoFirst <= prior.VideoLast || stats.AudioFirst <= prior.AudioLast) {
				return zero, fmt.Errorf("production segment timestamps regress on %s", stats.Track)
			}
			stats.SHA256 = hex.EncodeToString(out.Blob.Digest[:])
			evidence = append(evidence, stats)
			if err := registerBlob(ctx, tx, out.Blob); err != nil {
				return zero, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO segments(session_id,track,seq,source,source_epoch,source_ms,offset_ms,media_ms,
				duration_ms,gap_ms,blob_key) VALUES(?,?,?,'primary',1,?,?,?,?,0,?)`, p.Output, stats.Track, stream.NextSeq,
				job.Plan.Input.Source*ProductionFrameMS, stream.MediaDurationMS, stream.MediaDurationMS, duration, out.Blob.Key); err != nil {
				return zero, err
			}
		}
		if job.Take != "" {
			result, err := tx.ExecContext(ctx, `UPDATE production_takes SET state='published',actual_frame=?
				WHERE id=? AND production_id=? AND control_epoch=? AND state='queued'`, job.Start, job.Take, p.ID, p.ControlEpoch)
			if err != nil {
				return zero, err
			}
			rows, err := result.RowsAffected()
			if err != nil || rows != 1 {
				return zero, ErrFenced
			}
		}
		raw, err := json.Marshal(evidence)
		if err != nil {
			return zero, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE production_frames SET sequence_no=?,evidence=?,published_at=UTC_TIMESTAMP(6)
			WHERE production_id=? AND start_frame=?`, stream.NextSeq, raw, p.ID, job.Start); err != nil {
			return zero, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE productions SET program_slot=?,program_frame=program_frame+?,asrun_rev=asrun_rev+1 WHERE id=?`,
			job.Plan.Input.Slot, job.Frames, p.ID); err != nil {
			return zero, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET state='live',active_source='primary',source_epoch=1,revision=revision+1,
			next_seq=next_seq+1,next_offset_ms=next_offset_ms+?,source_end_ms=source_end_ms+?,media_duration_ms=media_duration_ms+? WHERE id=?`,
			duration, duration, duration, p.Output)
		return zero, err
	})
	return err
}

type AsRunEntry struct {
	Start       int64             `json:"actual_in_frame"`
	End         int64             `json:"actual_out_frame"`
	Sequence    *uint64           `json:"segment_sequence,omitempty"`
	Take        *string           `json:"take_id,omitempty"`
	Plan        FramePlan         `json:"plan"`
	Media       []ProductionTrack `json:"tracks"`
	PublishedAt *time.Time        `json:"published_at,omitempty"`
	Attempts    int               `json:"render_attempts"`
	Error       *string           `json:"last_error,omitempty"`
}

func (s *Store) ProductionAsRun(ctx context.Context, actor Identity, id string, after int64, limit int) (Production, []AsRunEntry, error) {
	p, err := authorizedProduction(ctx, s.db, actor, id, false, "owner", "director", "editor", "operator")
	if err != nil {
		return p, nil, err
	}
	if after < 0 || after > ProductionMaxFrames || limit < 1 || limit > 251 {
		return p, nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT start_frame,frame_count,sequence_no,take_id,source_plan,evidence,published_at,attempts,last_error
		FROM production_frames WHERE production_id=? AND start_frame>=? ORDER BY start_frame LIMIT ?`, id, after, limit)
	if err != nil {
		return p, nil, err
	}
	defer rows.Close()
	result := make([]AsRunEntry, 0)
	for rows.Next() {
		var entry AsRunEntry
		var rawPlan, rawMedia []byte
		var count int64
		if err := rows.Scan(&entry.Start, &count, &entry.Sequence, &entry.Take, &rawPlan, &rawMedia, &entry.PublishedAt, &entry.Attempts, &entry.Error); err != nil {
			return p, nil, err
		}
		entry.End = entry.Start + count
		if err := json.Unmarshal(rawPlan, &entry.Plan); err != nil {
			return p, nil, err
		}
		entry.Plan.Input.Blob = Blob{}
		if entry.Plan.Previous != nil {
			entry.Plan.Previous.Blob = Blob{}
		}
		if len(rawMedia) > 0 {
			if err := json.Unmarshal(rawMedia, &entry.Media); err != nil {
				return p, nil, err
			}
		}
		result = append(result, entry)
	}
	return p, result, rows.Err()
}
