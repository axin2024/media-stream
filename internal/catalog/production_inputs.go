package catalog

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

type ProductionSlot struct {
	ID         string     `json:"slot_id"`
	Title      string     `json:"title"`
	Kind       string     `json:"kind"`
	Audio      string     `json:"audio_channel"`
	ZeroMS     *int64     `json:"timecode_zero_ms,omitempty"`
	Alignment  int64      `json:"alignment_ms"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	LastFrame  *int64     `json:"last_source_frame,omitempty"`
	LostAt     *time.Time `json:"lost_at,omitempty"`
}

type SlotSettings struct {
	Title string `json:"title"`
	Kind  string `json:"kind"`
	Audio string `json:"audio_channel"`
}

type ClockObservation struct {
	SourceMS  int64 `json:"source_time_ms"`
	ProgramMS int64 `json:"program_time_ms"`
}

type SlotClock struct {
	Samples []ClockObservation `json:"samples"`
}

type ProductionInput struct {
	SourceFrame int64 `json:"source_frame"`
	StartFrame  int64 `json:"start_frame"`
	Frames      int   `json:"frame_count"`
	Replay      bool  `json:"replay"`
}

const productionSlotSelect = `SELECT slot_id,title,kind,audio_channel,timecode_zero_ms,alignment_ms,observed_at,last_source_frame,lost_at
	FROM production_slots WHERE production_id=?`

func scanProductionSlot(row scanner) (ProductionSlot, error) {
	var slot ProductionSlot
	err := row.Scan(&slot.ID, &slot.Title, &slot.Kind, &slot.Audio, &slot.ZeroMS, &slot.Alignment, &slot.ObservedAt, &slot.LastFrame, &slot.LostAt)
	return slot, missing(err)
}

func readProductionSlot(ctx context.Context, db queryer, id, slot string) (ProductionSlot, error) {
	return scanProductionSlot(db.QueryRowContext(ctx, productionSlotSelect+" AND slot_id=?", id, slot))
}

func (s *Store) ProductionSlots(ctx context.Context, actor Identity, id string) ([]ProductionSlot, error) {
	if _, err := s.Production(ctx, actor, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, productionSlotSelect+" ORDER BY slot_id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ProductionSlot, 0)
	for rows.Next() {
		slot, err := scanProductionSlot(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, slot)
	}
	return result, rows.Err()
}

func (s *Store) SetProductionSlot(ctx context.Context, actor Identity, id, slot string, in SlotSettings) (ProductionSlot, error) {
	if !identifier.MatchString(slot) || !strictText(in.Title, 160) || !slices.Contains([]string{"live", "clip", "slate"}, in.Kind) ||
		!slices.Contains([]string{"stereo", "left", "right", "mute"}, in.Audio) {
		return ProductionSlot{}, &ProductionInputError{"slot", "check the slot name, source type and audio channel"}
	}
	return transaction(ctx, s, func(tx *sql.Tx) (ProductionSlot, error) {
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "operator")
		if err != nil {
			return ProductionSlot{}, err
		}
		if p.State != "draft" {
			return ProductionSlot{}, ErrConflict
		}
		current, err := readProductionSlot(ctx, tx, id, slot)
		if err != nil && !errors.Is(err, ErrMissing) {
			return ProductionSlot{}, err
		}
		if (current.Kind != "" && current.Kind != in.Kind) || (slot == p.Fallback && in.Kind != "slate") {
			return ProductionSlot{}, &ProductionInputError{"kind", "the fallback slot is a slate and an existing slot keeps its type"}
		}
		if current.ID == "" {
			var count int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM production_slots WHERE production_id=?", id).Scan(&count); err != nil {
				return ProductionSlot{}, err
			}
			if count >= 16 {
				return ProductionSlot{}, &ProductionInputError{"slot", "a production can contain up to 16 slots"}
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO production_slots(production_id,slot_id,title,kind,audio_channel,timecode_zero_ms)
			VALUES(?,?,?,?,?,IF(?='live',NULL,0)) AS incoming ON DUPLICATE KEY UPDATE title=incoming.title,audio_channel=incoming.audio_channel`,
			id, slot, in.Title, in.Kind, in.Audio, in.Kind)
		if err != nil {
			return ProductionSlot{}, err
		}
		return readProductionSlot(ctx, tx, id, slot)
	})
}

func clockAlignment(input SlotClock) (int64, error) {
	if len(input.Samples) < 3 || len(input.Samples) > 9 {
		return 0, &ProductionInputError{"samples", "record between three and nine paired time observations"}
	}
	offsets := make([]int64, 0, len(input.Samples))
	var lastProgram, lastSource int64 = -1, -1
	for _, sample := range input.Samples {
		if sample.SourceMS <= lastSource || sample.ProgramMS <= lastProgram || sample.SourceMS > ProductionMaxFrames*ProductionFrameMS*2 ||
			sample.ProgramMS > ProductionMaxFrames*ProductionFrameMS {
			return 0, &ProductionInputError{"samples", "timestamps must be within bounds and strictly increasing"}
		}
		offsets = append(offsets, sample.SourceMS-sample.ProgramMS)
		lastSource, lastProgram = sample.SourceMS, sample.ProgramMS
	}
	slices.Sort(offsets)
	median := offsets[len(offsets)/2]
	if offsets[len(offsets)-1]-offsets[0] > ProductionFrameMS || median%ProductionFrameMS != 0 {
		return 0, &ProductionInputError{"samples", "observations require a stable frame-aligned offset with no jump above 40 ms"}
	}
	return median, nil
}

func (s *Store) CalibrateProductionSlot(ctx context.Context, actor Identity, id, slot string, input SlotClock) (ProductionSlot, error) {
	zero, err := clockAlignment(input)
	if err != nil {
		return ProductionSlot{}, err
	}
	return transaction(ctx, s, func(tx *sql.Tx) (ProductionSlot, error) {
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "operator")
		if err != nil {
			return ProductionSlot{}, err
		}
		current, err := readProductionSlot(ctx, tx, id, slot)
		if err != nil {
			return current, err
		}
		if (p.State != "draft" && p.State != "on_air") || current.Kind != "live" {
			return current, ErrConflict
		}
		if current.LastFrame != nil && current.ZeroMS != nil && *current.ZeroMS != zero {
			return current, &ProductionInputError{"samples", "the timecode changed after ingest; use a separately calibrated slot for a new clock"}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE production_slots SET timecode_zero_ms=?,alignment_ms=?,observed_at=UTC_TIMESTAMP(6)
			WHERE production_id=? AND slot_id=?`, zero, -zero, id, slot); err != nil {
			return current, err
		}
		if err := audit(ctx, tx, p.Output, "production_clock", actor.Label, map[string]any{"slot_id": slot, "zero_ms": zero}); err != nil {
			return current, err
		}
		return readProductionSlot(ctx, tx, id, slot)
	})
}

func (s *Store) ProductionInputAllowed(ctx context.Context, actor Identity, id, slot string) (ProductionSlot, error) {
	p, err := authorizedProduction(ctx, s.db, actor, id, false, "owner", "operator")
	if err != nil {
		return ProductionSlot{}, err
	}
	if p.State != "draft" && p.State != "on_air" {
		return ProductionSlot{}, ErrConflict
	}
	return readProductionSlot(ctx, s.db, id, slot)
}

func (s *Store) PutProductionInput(ctx context.Context, actor Identity, id, slot string, sourceFrame int64, frames int, blob Blob) (ProductionInput, error) {
	if sourceFrame < 0 || sourceFrame > ProductionMaxFrames*2 || frames < 1 || frames > 250 {
		return ProductionInput{}, &ProductionInputError{"source_frame", "source time or fragment length is outside the allowed range"}
	}
	return transaction(ctx, s, func(tx *sql.Tx) (ProductionInput, error) {
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "operator")
		if err != nil {
			return ProductionInput{}, err
		}
		active, err := readProductionSlot(ctx, tx, id, slot)
		if err != nil {
			return ProductionInput{}, err
		}
		if active.ZeroMS == nil {
			return ProductionInput{}, &ProductionInputError{"timecode", "calibrate the source before uploading its fragments"}
		}
		start := sourceFrame - *active.ZeroMS/ProductionFrameMS
		if start < 0 || start+int64(frames) > ProductionMaxFrames {
			return ProductionInput{}, &ProductionInputError{"source_frame", "fragment lies outside the production clock"}
		}
		result := ProductionInput{SourceFrame: sourceFrame, StartFrame: start, Frames: frames}
		var prior []byte
		var priorFrames int
		err = tx.QueryRowContext(ctx, `SELECT b.digest,i.frame_count FROM production_inputs i JOIN blobs b ON b.blob_key=i.blob_key
			WHERE i.production_id=? AND i.slot_id=? AND i.source_frame=?`, id, slot, sourceFrame).Scan(&prior, &priorFrames)
		if err == nil {
			if priorFrames != frames || !slices.Equal(prior, blob.Digest[:]) {
				return ProductionInput{}, ErrConflict
			}
			result.Replay = true
			return result, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ProductionInput{}, err
		}
		if p.State != "draft" && p.State != "on_air" {
			return ProductionInput{}, ErrConflict
		}
		var overlaps bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM production_inputs WHERE production_id=? AND slot_id=?
			AND start_frame BETWEEN ? AND ? AND start_frame+frame_count>?)`, id, slot, start-249, start+int64(frames)-1, start).Scan(&overlaps); err != nil {
			return ProductionInput{}, err
		}
		if overlaps {
			return ProductionInput{}, ErrConflict
		}
		if err := registerBlob(ctx, tx, blob); err != nil {
			return ProductionInput{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_inputs(production_id,slot_id,source_frame,frame_count,blob_key,start_frame)
			VALUES(?,?,?,?,?,?)`, id, slot, sourceFrame, frames, blob.Key, start); err != nil {
			return ProductionInput{}, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE production_slots SET last_source_frame=GREATEST(COALESCE(last_source_frame,0),?),
			lost_at=IF(last_source_frame<=?,NULL,lost_at) WHERE production_id=? AND slot_id=?`, sourceFrame, sourceFrame, id, slot)
		if err != nil {
			return ProductionInput{}, err
		}
		return result, addUsage(ctx, tx, p.Output, blob.Bytes, 0, 0, 0)
	})
}

func (s *Store) MarkProductionInputLost(ctx context.Context, actor Identity, id, slot string) error {
	_, err := transaction(ctx, s, func(tx *sql.Tx) (struct{}, error) {
		var zero struct{}
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "operator")
		if err != nil {
			return zero, err
		}
		source, err := readProductionSlot(ctx, tx, id, slot)
		if err != nil {
			return zero, err
		}
		if p.State != "on_air" || source.Kind != "live" {
			return zero, ErrConflict
		}
		if source.LostAt != nil {
			return zero, nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE production_slots SET lost_at=UTC_TIMESTAMP(6) WHERE production_id=? AND slot_id=?`, id, slot); err != nil {
			return zero, err
		}
		return zero, audit(ctx, tx, p.Output, "production_input_lost", actor.Label, map[string]any{"slot_id": slot, "frame": p.Frame})
	})
	return err
}

func (s *Store) ProductionPreviewMedia(ctx context.Context, actor Identity, id, slot string) (Blob, error) {
	if _, err := authorizedProduction(ctx, s.db, actor, id, false, "owner", "director", "operator"); err != nil {
		return Blob{}, err
	}
	var blob Blob
	var digest []byte
	err := s.db.QueryRowContext(ctx, `SELECT b.blob_key,b.bytes,b.digest FROM production_inputs i JOIN blobs b ON b.blob_key=i.blob_key
		WHERE i.production_id=? AND i.slot_id=? AND b.state='ready' ORDER BY i.start_frame DESC LIMIT 1`, id, slot).Scan(&blob.Key, &blob.Bytes, &digest)
	copy(blob.Digest[:], digest)
	return blob, missing(err)
}
