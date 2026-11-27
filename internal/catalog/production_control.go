package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"
)

type ControlRequest struct {
	Epoch   uint64 `json:"expected_control_epoch"`
	Seconds int    `json:"lease_seconds"`
}

type ControlConflict struct {
	Epoch      uint64     `json:"control_epoch"`
	Controller string     `json:"controller,omitempty"`
	Until      *time.Time `json:"control_until,omitempty"`
}

func (e *ControlConflict) Error() string { return "production control lease changed" }

func conflictFor(p Production) *ControlConflict {
	return &ControlConflict{p.ControlEpoch, p.Controller, p.ControlUntil}
}

func controlHeld(p Production, actor Identity, epoch uint64, now time.Time) bool {
	return p.ControlEpoch == epoch && p.ControlUntil != nil && p.ControlUntil.After(now) && bytes.Equal(p.controlKey, actor.credential[:])
}

func databaseNow(ctx context.Context, db queryer) (time.Time, error) {
	var now time.Time
	err := db.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now)
	return now, err
}

func (s *Store) ClaimProductionControl(ctx context.Context, actor Identity, id string, in ControlRequest) (Production, error) {
	if in.Seconds < 3 || in.Seconds > 120 {
		return Production{}, &ProductionInputError{"lease_seconds", "provide a duration between 3 and 120 seconds"}
	}
	return transaction(ctx, s, func(tx *sql.Tx) (Production, error) {
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "director", "operator")
		if err != nil {
			return p, err
		}
		if p.State == "sealed" || p.State == "ending" {
			return p, ErrConflict
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return p, err
		}
		epoch := p.ControlEpoch
		if in.Epoch != epoch || (p.ControlUntil != nil && p.ControlUntil.After(now) && !bytes.Equal(p.controlKey, actor.credential[:])) {
			return p, conflictFor(p)
		}
		if !controlHeld(p, actor, epoch, now) {
			epoch++
			if _, err := tx.ExecContext(ctx, `UPDATE production_takes SET state='fenced'
				WHERE production_id=? AND control_epoch<? AND state='queued'`, id, epoch); err != nil {
				return p, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE f FROM production_frames f JOIN production_takes t ON t.id=f.take_id
				WHERE f.production_id=? AND f.sequence_no IS NULL AND t.state='fenced'`, id); err != nil {
				return p, err
			}
			if err := audit(ctx, tx, p.Output, "production_control", actor.Label, map[string]any{"id": id, "epoch": epoch}); err != nil {
				return p, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE productions SET control_epoch=?,control_credential=?,controller=?,control_until=? WHERE id=?`,
			epoch, actor.credential[:], actor.Label, now.Add(time.Duration(in.Seconds)*time.Second), id); err != nil {
			return p, err
		}
		return readProduction(ctx, tx, id, false)
	})
}

func (s *Store) SetProductionPreview(ctx context.Context, actor Identity, id, slot string, epoch uint64) (Production, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (Production, error) {
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "director", "operator")
		if err != nil {
			return p, err
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return p, err
		}
		if !controlHeld(p, actor, epoch, now) {
			return p, conflictFor(p)
		}
		if p.State != "draft" && p.State != "on_air" {
			return p, ErrConflict
		}
		if _, err := readProductionSlot(ctx, tx, id, slot); err != nil {
			return p, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE productions SET preview_slot=? WHERE id=?", slot, id); err != nil {
			return p, err
		}
		p.Preview = slot
		return p, nil
	})
}

func (s *Store) StartProduction(ctx context.Context, actor Identity, id string, epoch uint64) (Production, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (Production, error) {
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "director", "operator")
		if err != nil {
			return p, err
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return p, err
		}
		if !controlHeld(p, actor, epoch, now) {
			return p, conflictFor(p)
		}
		if p.State == "on_air" {
			return p, nil
		}
		rundown, err := readRundown(ctx, tx, p)
		if err != nil {
			return p, err
		}
		fallback, err := readProductionSlot(ctx, tx, id, p.Fallback)
		if errors.Is(err, ErrMissing) || (err == nil && fallback.Kind != "slate") {
			return p, &ProductionInputError{"fallback_slot", "configure a continuity slate before starting"}
		}
		if err != nil {
			return p, err
		}
		var available bool
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(UTC_TIMESTAMP(6)>=available_from AND UTC_TIMESTAMP(6)<available_until,false)
			FROM channels WHERE tenant=? AND channel=? AND session_id=?`, p.Tenant, p.Channel, p.Output).Scan(&available); err != nil {
			return p, missing(err)
		}
		if p.State != "draft" || len(rundown.Items) == 0 || !available {
			return p, ErrConflict
		}
		if _, err := tx.ExecContext(ctx, "UPDATE productions SET state='on_air',started_at=? WHERE id=?", now, id); err != nil {
			return p, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE sessions SET started_at=?,revision=revision+1 WHERE id=?", now, p.Output); err != nil {
			return p, err
		}
		if err := audit(ctx, tx, p.Output, "production_started", actor.Label, map[string]any{"id": id, "rundown_rev": p.RundownRev}); err != nil {
			return p, err
		}
		return readProduction(ctx, tx, id, false)
	})
}

func (s *Store) EndProduction(ctx context.Context, actor Identity, id string, epoch uint64) (Production, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (Production, error) {
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "director", "operator")
		if err != nil {
			return p, err
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return p, err
		}
		if !controlHeld(p, actor, epoch, now) {
			return p, conflictFor(p)
		}
		if p.State == "ending" || p.State == "sealed" {
			return p, nil
		}
		if p.State != "on_air" && p.State != "draft" {
			return p, ErrConflict
		}
		if _, err := tx.ExecContext(ctx, "UPDATE productions SET state='ending' WHERE id=?", id); err != nil {
			return p, err
		}
		if err := audit(ctx, tx, p.Output, "production_ending", actor.Label, map[string]any{"id": id, "frame": p.Frame}); err != nil {
			return p, err
		}
		return readProduction(ctx, tx, id, false)
	})
}

type TakeRequest struct {
	Slot       string `json:"slot_id"`
	Epoch      uint64 `json:"expected_control_epoch"`
	Transition string `json:"transition"`
	Frames     int    `json:"transition_frames,omitempty"`
	Command    string `json:"command_id"`
}

type ProductionTake struct {
	ID         string `json:"take_id"`
	Slot       string `json:"slot_id"`
	Epoch      uint64 `json:"control_epoch"`
	Transition string `json:"transition"`
	Frames     int    `json:"transition_frames"`
	Command    string `json:"command_id"`
	State      string `json:"state"`
	Planned    int64  `json:"planned_frame"`
	Actual     *int64 `json:"actual_frame,omitempty"`
	PTS        *int64 `json:"actual_pts,omitempty"`
	Program    string `json:"program_output"`
	Actor      string `json:"actor"`
}

const takeSelect = `SELECT id,slot_id,control_epoch,transition,transition_frames,command_id,state,planned_frame,actual_frame,actor,request_digest
	FROM production_takes WHERE production_id=?`

func scanProductionTake(row scanner, output string) (ProductionTake, []byte, error) {
	t := ProductionTake{Program: output}
	var fingerprint []byte
	err := row.Scan(&t.ID, &t.Slot, &t.Epoch, &t.Transition, &t.Frames, &t.Command, &t.State, &t.Planned, &t.Actual, &t.Actor, &fingerprint)
	if t.Actual != nil {
		pts := *t.Actual * (ProductionTimebase / ProductionFPS)
		t.PTS = &pts
	}
	return t, fingerprint, missing(err)
}

func (s *Store) TakeProduction(ctx context.Context, actor Identity, id, request string, in TakeRequest) (ProductionTake, error) {
	if !requestKey.MatchString(request) || !requestKey.MatchString(in.Command) || !identifier.MatchString(in.Slot) || in.Epoch == 0 {
		return ProductionTake{}, &ProductionInputError{"command_id", "provide the slot ID, control epoch, command ID and idempotency key"}
	}
	if !((in.Transition == "cut" && in.Frames == 0) || (in.Transition == "dissolve" && in.Frames >= 2 && in.Frames <= ProductionFPS)) {
		return ProductionTake{}, &ProductionInputError{"transition", "use cut or dissolve with 2 to 25 frames"}
	}
	return transaction(ctx, s, func(tx *sql.Tx) (ProductionTake, error) {
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "director", "operator")
		if err != nil {
			return ProductionTake{}, err
		}
		fingerprint := digest(struct {
			TakeRequest
			Principal [32]byte
		}{in, actor.credential})
		prior, oldDigest, err := scanProductionTake(tx.QueryRowContext(ctx, takeSelect+" AND request_key=?", id, request), p.Output)
		if errors.Is(err, ErrMissing) {
			prior, oldDigest, err = scanProductionTake(tx.QueryRowContext(ctx, takeSelect+" AND control_epoch=? AND command_id=?", id,
				in.Epoch, in.Command), p.Output)
		}
		if err == nil {
			if !bytes.Equal(fingerprint[:], oldDigest) {
				return ProductionTake{}, ErrConflict
			}
			return prior, nil
		}
		if !errors.Is(err, ErrMissing) {
			return ProductionTake{}, err
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return ProductionTake{}, err
		}
		if !controlHeld(p, actor, in.Epoch, now) {
			return ProductionTake{}, conflictFor(p)
		}
		if p.State != "on_air" || p.Preview != in.Slot {
			return ProductionTake{}, &ProductionInputError{"slot_id", "put the selected slot in preview during the active production"}
		}
		slot, err := readProductionSlot(ctx, tx, id, in.Slot)
		if err != nil {
			return ProductionTake{}, err
		}
		if slot.ZeroMS == nil || slot.LostAt != nil || (slot.LastFrame == nil && slot.Kind != "slate") {
			return ProductionTake{}, &ProductionInputError{"slot_id", "source media with a valid clock is required before taking it to air"}
		}
		frame, err := productionCommittedUntil(ctx, tx, p)
		if err != nil {
			return ProductionTake{}, err
		}
		var pending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM production_takes WHERE production_id=? AND state='queued')`, id).Scan(&pending); err != nil {
			return ProductionTake{}, err
		}
		if pending {
			return ProductionTake{}, ErrConflict
		}
		plan, err := readRundown(ctx, tx, p)
		if err != nil {
			return ProductionTake{}, err
		}
		for _, item := range plan.Items {
			if item.Fixed && item.In <= frame && frame < item.Out {
				return ProductionTake{}, &ProductionInputError{"slot_id", "manual takes are held during a fixed programme item"}
			}
		}
		take := ProductionTake{ID: key(), Slot: in.Slot, Epoch: in.Epoch, Transition: in.Transition, Frames: in.Frames,
			Command: in.Command, State: "queued", Planned: frame, Program: p.Output, Actor: actor.Label}
		_, err = tx.ExecContext(ctx, `INSERT INTO production_takes(id,production_id,control_epoch,command_id,request_key,request_digest,
			slot_id,transition,transition_frames,state,planned_frame,actor) VALUES(?,?,?,?,?,?,?,?,?,'queued',?,?)`,
			take.ID, id, in.Epoch, in.Command, request, fingerprint[:], in.Slot, in.Transition, in.Frames, frame, actor.Label)
		return take, err
	})
}

func (s *Store) ProductionTake(ctx context.Context, actor Identity, id, take string) (ProductionTake, error) {
	p, err := s.Production(ctx, actor, id)
	if err != nil {
		return ProductionTake{}, err
	}
	t, _, err := scanProductionTake(s.db.QueryRowContext(ctx, takeSelect+" AND id=?", id, take), p.Output)
	return t, err
}
