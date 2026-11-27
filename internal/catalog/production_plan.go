package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type FrameInput struct {
	Slot    string `json:"slot_id"`
	Blob    Blob   `json:"blob"`
	Offset  int64  `json:"input_offset_frames"`
	Source  int64  `json:"source_frame"`
	Left    int    `json:"available_frames"`
	Audio   string `json:"audio_channel"`
	Loop    bool   `json:"loop,omitempty"`
	BuiltIn bool   `json:"built_in_slate,omitempty"`
}

type FramePlan struct {
	RundownRev uint64      `json:"rundown_rev"`
	Item       string      `json:"item_id,omitempty"`
	ExpectedIn int64       `json:"planned_in_frame"`
	Intended   string      `json:"intended_slot,omitempty"`
	Reason     string      `json:"reason"`
	Input      FrameInput  `json:"source"`
	Previous   *FrameInput `json:"transition_from,omitempty"`
	FadeFrames int         `json:"dissolve_frames,omitempty"`
	Overlays   []Overlay   `json:"overlays"`
}

type ProductionJob struct {
	Production string
	Output     string
	Preset     string
	Start      int64
	Frames     int
	Claim      string
	Take       string
	Attempt    int
	Plan       FramePlan
}

const renderLease = 25 * time.Second

func (s *Store) ActiveProductions(ctx context.Context, after string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM productions WHERE id>? AND state IN ('on_air','ending') ORDER BY id LIMIT 128`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) ClaimProductionFrames(ctx context.Context, id string) (ProductionJob, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (ProductionJob, error) {
		p, err := readProduction(ctx, tx, id, true)
		if err != nil || (p.State != "on_air" && p.State != "ending") {
			return ProductionJob{}, err
		}
		stream, err := readSession(ctx, tx, p.Output, true)
		if err != nil {
			return ProductionJob{}, err
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return ProductionJob{}, err
		}
		job := ProductionJob{Production: id, Output: p.Output, Preset: stream.Preset, Start: p.Frame}
		var lease time.Time
		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT frame_count,source_plan,COALESCE(take_id,''),lease_until,attempts
			FROM production_frames WHERE production_id=? AND start_frame=? AND sequence_no IS NULL`, id, p.Frame).
			Scan(&job.Frames, &raw, &job.Take, &lease, &job.Attempt)
		if err == nil {
			if lease.After(now) {
				return ProductionJob{}, nil
			}
			if err := json.Unmarshal(raw, &job.Plan); err != nil {
				return ProductionJob{}, err
			}
			job.Attempt++
			job.Claim = key()
			if job.Attempt == 4 && !job.Plan.Input.BuiltIn {
				if err := failProductionTake(ctx, tx, id, job.Take); err != nil {
					return ProductionJob{}, err
				}
				job.Take = ""
				job.Plan.Input = builtInSlate(p.Fallback)
				job.Plan.Previous, job.Plan.FadeFrames = nil, 0
				job.Plan.Reason = "source_render_failed"
			}
			if job.Attempt >= 8 {
				return ProductionJob{}, sealProduction(ctx, tx, p, stream, "renderer_unavailable")
			}
			raw, err = json.Marshal(job.Plan)
			if err != nil {
				return ProductionJob{}, err
			}
			_, err = tx.ExecContext(ctx, `UPDATE production_frames SET claim=?,lease_until=?,attempts=?,source_plan=?,take_id=NULLIF(?,'')
				WHERE production_id=? AND start_frame=?`, job.Claim, now.Add(renderLease), job.Attempt, raw, job.Take, id, p.Frame)
			return job, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ProductionJob{}, err
		}
		if p.State == "ending" {
			return ProductionJob{}, sealProduction(ctx, tx, p, stream, "operator")
		}
		plan, err := readRundown(ctx, tx, p)
		if err != nil {
			return ProductionJob{}, err
		}
		var until time.Time
		if err := tx.QueryRowContext(ctx, `SELECT available_until FROM channels WHERE tenant=? AND channel=?`, p.Tenant, p.Channel).Scan(&until); err != nil {
			return ProductionJob{}, err
		}
		if !now.Before(until) || len(plan.Items) == 0 || p.Frame >= plan.Items[len(plan.Items)-1].Out {
			return ProductionJob{}, sealProduction(ctx, tx, p, stream, "rundown_end")
		}
		job.Frames = min(stream.SegmentMS/ProductionFrameMS, int(plan.Items[len(plan.Items)-1].Out-p.Frame))
		_, job.Frames = frameRundown(plan.Items, p.Frame, job.Frames)
		// Ingest is allowed a fixed lookahead to include the complete source range for each interval.
		readyAt := p.Started.Add(time.Duration(p.Frame+int64(job.Frames))*ProductionFrameMS*time.Millisecond + 750*time.Millisecond)
		if now.Before(readyAt) {
			return ProductionJob{}, nil
		}
		if err := planProductionFrame(ctx, tx, p, plan, now, &job); err != nil {
			return ProductionJob{}, err
		}
		job.Claim, job.Attempt = key(), 1
		raw, err = json.Marshal(job.Plan)
		if err != nil {
			return ProductionJob{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO production_frames(production_id,start_frame,frame_count,source_plan,take_id,claim,lease_until)
			VALUES(?,?,?,?,NULLIF(?,''),?,?)`, id, p.Frame, job.Frames, raw, job.Take, job.Claim, now.Add(renderLease))
		return job, err
	})
}

func sealProduction(ctx context.Context, tx *sql.Tx, p Production, stream Session, reason string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE production_takes SET state='failed' WHERE production_id=? AND state='queued'`, p.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE productions SET state='sealed',sealed_at=UTC_TIMESTAMP(6),asrun_rev=asrun_rev+1
		WHERE id=?`, p.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at=UTC_TIMESTAMP(6) WHERE id=?`, p.Output); err != nil {
		return err
	}
	if err := audit(ctx, tx, p.Output, "production_sealed", "renderer", map[string]any{"reason": reason, "program_frame": p.Frame}); err != nil {
		return err
	}
	return archive(ctx, tx, stream)
}

func frameRundown(items []RundownItem, frame int64, count int) (RundownItem, int) {
	for _, item := range items {
		if item.Out <= frame {
			continue
		}
		if frame < item.In {
			return RundownItem{}, min(count, int(item.In-frame))
		}
		return item, min(count, int(item.Out-frame))
	}
	return RundownItem{}, count
}

func planProductionFrame(ctx context.Context, tx *sql.Tx, p Production, rundown Rundown, now time.Time, job *ProductionJob) error {
	item, frames := frameRundown(rundown.Items, p.Frame, job.Frames)
	job.Frames = frames
	job.Plan = FramePlan{RundownRev: rundown.Revision, Item: item.ID, ExpectedIn: item.In, Intended: item.Slot, Reason: "rundown"}
	if p.ControlUntil == nil || !p.ControlUntil.After(now) {
		if _, err := tx.ExecContext(ctx, `UPDATE production_takes SET state='fenced' WHERE production_id=? AND state='queued'`, p.ID); err != nil {
			return err
		}
	}
	var selected ProductionTake
	var armed bool
	if !item.Fixed && item.ID != "" {
		var err error
		selected, _, err = scanProductionTake(tx.QueryRowContext(ctx, takeSelect+` AND
			((state='queued' AND planned_frame<=? AND control_epoch=?) OR (state='published' AND actual_frame>=? AND actual_frame<=?))
			ORDER BY (state='queued') DESC,actual_frame DESC LIMIT 1`, p.ID, p.Frame, p.ControlEpoch, item.In, p.Frame), p.Output)
		if err != nil && !errors.Is(err, ErrMissing) {
			return err
		}
		armed = selected.State == "queued"
	}
	if selected.ID != "" {
		job.Plan.Intended, job.Plan.Reason = selected.Slot, "director"
	}
	input, limit, err := plannedInput(ctx, tx, p, item, selected, job.Plan.Intended, job.Frames)
	if err != nil {
		return err
	}
	if armed && input.Slot == selected.Slot {
		if selected.Transition == "dissolve" && p.Program != "" && p.Program != input.Slot {
			previous, previousLimit, err := plannedInput(ctx, tx, p, item, ProductionTake{}, p.Program, job.Frames)
			if err != nil {
				return err
			}
			if previous.Slot == "" && p.Program == p.Fallback {
				previous, previousLimit, err = continuitySlate(ctx, tx, p, job.Frames)
				if err != nil {
					return err
				}
			}
			if previous.Slot == "" || min(previousLimit, limit, job.Frames) < selected.Frames {
				input.Slot = ""
			} else {
				job.Plan.Previous, job.Plan.FadeFrames = &previous, selected.Frames
			}
		}
		if input.Slot != "" {
			job.Take = selected.ID
		}
	}
	if armed && job.Take == "" {
		if err := failProductionTake(ctx, tx, p.ID, selected.ID); err != nil {
			return err
		}
		job.Plan.Intended, job.Plan.Reason = item.Slot, "take_source_unavailable"
		input, limit, err = plannedInput(ctx, tx, p, item, ProductionTake{}, item.Slot, job.Frames)
		if err != nil {
			return err
		}
	}
	if input.Slot == "" {
		job.Frames = min(job.Frames, limit)
		input, limit, err = continuitySlate(ctx, tx, p, job.Frames)
		if err != nil {
			return err
		}
		job.Plan.Reason = "source_unavailable"
		if item.ID == "" {
			job.Plan.Reason = "rundown_gap"
		}
	}
	job.Plan.Input = input
	job.Frames = min(job.Frames, limit)
	job.Plan.Overlays, err = frameOverlays(ctx, tx, p.ID, p.Frame, p.Frame+int64(job.Frames))
	return err
}

func plannedInput(ctx context.Context, tx *sql.Tx, p Production, item RundownItem, take ProductionTake, slotID string, limit int) (FrameInput, int, error) {
	if slotID == "" {
		return FrameInput{}, limit, nil
	}
	slot, err := readProductionSlot(ctx, tx, p.ID, slotID)
	if err != nil {
		return FrameInput{}, limit, err
	}
	if slot.ZeroMS == nil {
		return FrameInput{}, limit, nil
	}
	if slotID == p.Fallback && slot.Kind == "slate" && item.Slot != slotID {
		return continuitySlate(ctx, tx, p, limit)
	}
	wanted := p.Frame
	if slot.Kind != "live" {
		switch {
		case take.ID != "" && take.Slot == slot.ID && take.Actual != nil:
			wanted = p.Frame - *take.Actual
		case take.ID != "" && take.Slot == slot.ID:
			wanted = 0
		default:
			wanted = item.SourceIn + p.Frame - item.In
		}
	}
	input, next, err := productionFragment(ctx, tx, p.ID, slot, wanted)
	if input.Slot == "" && slot.Kind == "slate" && err == nil {
		return continuitySlate(ctx, tx, p, limit)
	}
	return input, min(limit, next), err
}

func productionFragment(ctx context.Context, db queryer, id string, slot ProductionSlot, wanted int64) (FrameInput, int, error) {
	var in FrameInput
	var base, source int64
	var count int
	var sha []byte
	err := db.QueryRowContext(ctx, `SELECT i.start_frame,i.source_frame,i.frame_count,b.blob_key,b.bytes,b.digest
		FROM production_inputs i JOIN blobs b ON b.blob_key=i.blob_key WHERE i.production_id=? AND i.slot_id=?
		AND i.start_frame BETWEEN ? AND ? AND i.start_frame+i.frame_count>? AND b.state='ready'
		ORDER BY i.start_frame DESC LIMIT 1`, id, slot.ID, wanted-249, wanted, wanted).
		Scan(&base, &source, &count, &in.Blob.Key, &in.Blob.Bytes, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		var next *int64
		err = db.QueryRowContext(ctx, `SELECT MIN(i.start_frame) FROM production_inputs i JOIN blobs b ON b.blob_key=i.blob_key
			WHERE i.production_id=? AND i.slot_id=? AND i.start_frame>? AND b.state='ready'`, id, slot.ID, wanted).Scan(&next)
		if next != nil {
			return in, int(*next - wanted), err
		}
		return in, ProductionMaxFrames, err
	}
	if err != nil {
		return in, 0, err
	}
	copy(in.Blob.Digest[:], sha)
	in.Slot, in.Audio = slot.ID, slot.Audio
	in.Offset = wanted - base
	in.Source = source + in.Offset
	in.Left = count - int(in.Offset)
	return in, in.Left, nil
}

func continuitySlate(ctx context.Context, tx *sql.Tx, p Production, limit int) (FrameInput, int, error) {
	slot, err := readProductionSlot(ctx, tx, p.ID, p.Fallback)
	if err != nil {
		return FrameInput{}, 0, err
	}
	input, _, err := productionFragment(ctx, tx, p.ID, slot, 0)
	if input.Slot == "" {
		return builtInSlate(slot.ID), limit, err
	}
	input.Offset = p.Frame % int64(input.Left)
	input.Source = input.Offset
	input.Loop = true
	return input, limit, err
}

func builtInSlate(slot string) FrameInput {
	return FrameInput{Slot: slot, Audio: "mute", BuiltIn: true}
}

func failProductionTake(ctx context.Context, tx *sql.Tx, id, take string) error {
	if take == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, "UPDATE production_takes SET state='failed' WHERE production_id=? AND id=? AND state='queued'", id, take)
	return err
}
