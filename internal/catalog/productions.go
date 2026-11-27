package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
	_ "time/tzdata"
)

type Production struct {
	ID           string     `json:"id"`
	Tenant       string     `json:"tenant"`
	Channel      string     `json:"channel"`
	Output       string     `json:"program_output"`
	Title        string     `json:"title"`
	Timezone     string     `json:"timezone"`
	State        string     `json:"state"`
	RundownRev   uint64     `json:"rundown_rev"`
	AsRunRev     uint64     `json:"asrun_rev"`
	ControlEpoch uint64     `json:"control_epoch"`
	Controller   string     `json:"controller,omitempty"`
	ControlUntil *time.Time `json:"control_until,omitempty"`
	Program      string     `json:"program_slot,omitempty"`
	Preview      string     `json:"preview_slot,omitempty"`
	Fallback     string     `json:"fallback_slot"`
	Frame        int64      `json:"program_frame"`
	Started      *time.Time `json:"started_at,omitempty"`
	Sealed       *time.Time `json:"sealed_at,omitempty"`
	FPS          int        `json:"fps"`
	Timebase     int        `json:"pts_per_second"`
	AudioRate    int        `json:"audio_sample_rate"`
	controlKey   []byte
}

type ProductionOpening struct {
	Session  string `json:"session_id"`
	Title    string `json:"title"`
	Timezone string `json:"timezone"`
	Fallback string `json:"fallback_slot"`
}

const productionColumns = `p.id,s.tenant,s.channel,p.session_id,p.title,p.timezone,p.state,p.rundown_rev,p.asrun_rev,
	p.control_epoch,p.controller,p.control_until,p.program_slot,p.preview_slot,p.fallback_slot,p.program_frame,p.started_at,p.sealed_at,p.control_credential`

func scanProduction(row scanner) (Production, error) {
	p := Production{FPS: ProductionFPS, Timebase: ProductionTimebase, AudioRate: ProductionAudioRate}
	err := row.Scan(&p.ID, &p.Tenant, &p.Channel, &p.Output, &p.Title, &p.Timezone, &p.State, &p.RundownRev, &p.AsRunRev,
		&p.ControlEpoch, &p.Controller, &p.ControlUntil, &p.Program, &p.Preview, &p.Fallback, &p.Frame, &p.Started, &p.Sealed, &p.controlKey)
	return p, missing(err)
}

func readProduction(ctx context.Context, db queryer, id string, lock bool) (Production, error) {
	query := `SELECT ` + productionColumns + ` FROM productions p JOIN sessions s ON s.id=p.session_id WHERE p.id=?`
	if lock {
		query += " FOR UPDATE OF p"
	}
	return scanProduction(db.QueryRowContext(ctx, query, id))
}

func authorizedProduction(ctx context.Context, db queryer, actor Identity, id string, lock bool, roles ...string) (Production, error) {
	p, err := readProduction(ctx, db, id, lock)
	if err != nil {
		return p, err
	}
	if !actor.Allows(p.Tenant, p.Channel, p.Output, roles...) {
		return Production{}, ErrMissing
	}
	return p, nil
}

func (s *Store) Production(ctx context.Context, actor Identity, id string) (Production, error) {
	return authorizedProduction(ctx, s.db, actor, id, false, "owner", "director", "editor", "operator", "viewer")
}

func (s *Store) Productions(ctx context.Context, actor Identity, after string) ([]Production, error) {
	if actor.Role == "push" || (after != "" && !identifier.MatchString(after)) {
		return nil, ErrDenied
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+productionColumns+` FROM productions p JOIN sessions s ON s.id=p.session_id
		WHERE s.tenant=? AND (?='' OR s.channel=?) AND (?='' OR s.id=?) AND p.id>? ORDER BY p.id LIMIT 101`,
		actor.Tenant, actor.Channel, actor.Channel, actor.Session, actor.Session, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]Production, 0)
	for rows.Next() {
		p, err := scanProduction(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, p)
	}
	return values, rows.Err()
}

func (s *Store) CreateProduction(ctx context.Context, actor Identity, request string, in ProductionOpening) (Production, error) {
	if !requestKey.MatchString(request) || !identifier.MatchString(in.Fallback) || !strictText(in.Title, 160) ||
		len(in.Timezone) > 64 || in.Timezone == "Local" || in.Timezone == "" {
		return Production{}, ErrInvalid
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		return Production{}, &ProductionInputError{"timezone", "provide a valid IANA time zone"}
	}
	return transaction(ctx, s, func(tx *sql.Tx) (Production, error) {
		stream, err := readSession(ctx, tx, in.Session, true)
		if err != nil {
			return Production{}, err
		}
		if !actor.Allows(stream.Tenant, stream.Channel, stream.ID, "owner") {
			return Production{}, ErrMissing
		}
		fingerprint := digest(in)
		id, replay, err := receipt(ctx, tx, actor.Tenant, "production", request, fingerprint)
		if err != nil {
			return Production{}, err
		}
		if replay {
			return readProduction(ctx, tx, id, false)
		}
		var hasInput bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM input_fragments WHERE session_id=?)", in.Session).Scan(&hasInput); err != nil {
			return Production{}, err
		}
		if stream.State != "waiting" || stream.NextSeq != 0 || hasInput {
			return Production{}, ErrConflict
		}
		id = key()
		if _, err := tx.ExecContext(ctx, `INSERT INTO productions(id,session_id,title,timezone,fallback_slot) VALUES(?,?,?,?,?)`,
			id, in.Session, in.Title, in.Timezone, in.Fallback); err != nil {
			if duplicate(err) {
				return Production{}, ErrConflict
			}
			return Production{}, err
		}
		if err := saveReceipt(ctx, tx, actor.Tenant, "production", request, fingerprint, id); err != nil {
			return Production{}, err
		}
		return readProduction(ctx, tx, id, false)
	})
}

func productionOwnsSession(ctx context.Context, db queryer, id string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM productions WHERE session_id=?)", id).Scan(&exists)
	return exists, err
}

func readRundown(ctx context.Context, db queryer, p Production) (Rundown, error) {
	result := Rundown{Revision: p.RundownRev, Items: []RundownItem{}, Conflict: []RundownConflict{}}
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT items FROM production_rundowns WHERE production_id=? AND revision=?`, p.ID, p.RundownRev).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) && p.RundownRev == 1 {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	return result, json.Unmarshal(raw, &result.Items)
}

func (s *Store) Rundown(ctx context.Context, actor Identity, id string) (Rundown, error) {
	p, err := s.Production(ctx, actor, id)
	if err != nil {
		return Rundown{}, err
	}
	return readRundown(ctx, s.db, p)
}

func (s *Store) ChangeRundown(ctx context.Context, actor Identity, id string, in RundownChange) (Rundown, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (Rundown, error) {
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "director")
		if err != nil {
			return Rundown{}, err
		}
		current, err := readRundown(ctx, tx, p)
		if err != nil {
			return Rundown{}, err
		}
		if p.RundownRev != in.Expected || p.State == "ending" || p.State == "sealed" {
			return current, ErrConflict
		}
		items := in.Items
		if in.Extend != "" {
			if len(items) != 0 {
				return current, &ProductionInputError{"items", "an extension uses the currently confirmed rundown"}
			}
			items = current.Items
		}
		candidate, err := calculateRundown(items, in.Extend, in.By)
		candidate.Revision = p.RundownRev
		if err != nil || len(candidate.Conflict) > 0 {
			return candidate, err
		}
		limit, err := productionCommittedUntil(ctx, tx, p)
		if err != nil {
			return current, err
		}
		if !slices.Equal(rundownBefore(current.Items, limit), rundownBefore(candidate.Items, limit)) {
			return current, &ProductionInputError{"items", "the rendered or reserved part of the rundown cannot be edited"}
		}
		for _, item := range candidate.Items {
			slot, err := readProductionSlot(ctx, tx, id, item.Slot)
			if errors.Is(err, ErrMissing) || (err == nil && slot.Kind != item.Kind && !(item.Kind == "advert" && slot.Kind == "clip")) {
				return current, &ProductionInputError{"items", "rundown items must refer to a compatible input slot"}
			}
			if err != nil {
				return current, err
			}
		}
		if slices.Equal(current.Items, candidate.Items) {
			return current, nil
		}
		candidate.Revision++
		encoded, err := json.Marshal(candidate.Items)
		if err != nil {
			return current, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_rundowns(production_id,revision,items,actor) VALUES(?,?,?,?)`,
			id, candidate.Revision, encoded, actor.Label); err != nil {
			return current, err
		}
		_, err = tx.ExecContext(ctx, "UPDATE productions SET rundown_rev=? WHERE id=?", candidate.Revision, id)
		return candidate, err
	})
}

func rundownBefore(items []RundownItem, frame int64) []RundownItem {
	var before []RundownItem
	for _, item := range items {
		if item.In >= frame {
			break
		}
		item.Out = min(item.Out, frame)
		before = append(before, item)
	}
	return before
}

func productionCommittedUntil(ctx context.Context, db queryer, p Production) (int64, error) {
	var end int64
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(start_frame+frame_count),?) FROM production_frames
		WHERE production_id=? AND start_frame=?`, p.Frame, p.ID, p.Frame).Scan(&end)
	return end, err
}
