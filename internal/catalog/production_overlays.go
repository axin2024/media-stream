package catalog

import (
	"context"
	"database/sql"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"
)

type OverlayRequest struct {
	Template string `json:"template"`
	Content  string `json:"text"`
	Size     int    `json:"font_size"`
	Start    *int64 `json:"on_frame,omitempty"`
	Length   int    `json:"duration_frames"`
}

func (o OverlayRequest) Validate() error {
	if o.Template != "lower_third" && o.Template != "logo" && o.Template != "timer" {
		return &ProductionInputError{"template", "choose lower_third, logo or timer"}
	}
	if len(o.Content) > 320 || !utf8.ValidString(o.Content) || strings.IndexFunc(o.Content, unicode.IsControl) >= 0 ||
		(o.Template != "timer" && strings.TrimSpace(o.Content) == "") {
		return &ProductionInputError{"text", "provide one line of text up to 320 UTF-8 bytes"}
	}
	if o.Size < 18 || o.Size > 72 {
		return &ProductionInputError{"font_size", "use a 720p font size between 18 and 72 px"}
	}
	if o.Length < 1 || o.Length > 10*60*ProductionFPS || (o.Start != nil && (*o.Start < 0 || *o.Start > ProductionMaxFrames-int64(o.Length))) {
		return &ProductionInputError{"duration_frames", "provide a valid interval up to ten minutes"}
	}
	return nil
}

type Overlay struct {
	ID       string  `json:"overlay_id"`
	Template string  `json:"template"`
	Content  string  `json:"text"`
	Font     string  `json:"font_sha256"`
	Size     int     `json:"font_size"`
	On       int64   `json:"on_frame"`
	Off      int64   `json:"off_frame"`
	Planned  int64   `json:"expected_off_frame"`
	Actor    string  `json:"actor"`
	Cleared  *string `json:"cleared_by,omitempty"`
}

const overlaySelect = `SELECT id,template,content,font_digest,font_size,on_frame,off_frame,expected_off_frame,actor,cleared_by
	FROM production_overlays WHERE production_id=?`

func queryOverlays(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, stmt string, args ...any) ([]Overlay, error) {
	rows, err := db.QueryContext(ctx, overlaySelect+stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Overlay, 0)
	for rows.Next() {
		var o Overlay
		var raw []byte
		if err := rows.Scan(&o.ID, &o.Template, &o.Content, &raw, &o.Size, &o.On, &o.Off, &o.Planned, &o.Actor, &o.Cleared); err != nil {
			return nil, err
		}
		o.Font = hex.EncodeToString(raw)
		result = append(result, o)
	}
	return result, rows.Err()
}

func frameOverlays(ctx context.Context, tx *sql.Tx, id string, from, to int64) ([]Overlay, error) {
	return queryOverlays(ctx, tx, " AND on_frame<? AND off_frame>? AND off_frame>on_frame ORDER BY template,on_frame,id", id, to, from)
}

func (s *Store) ProductionOverlays(ctx context.Context, actor Identity, id string, after string) ([]Overlay, error) {
	if _, err := s.Production(ctx, actor, id); err != nil {
		return nil, err
	}
	if after != "" && !identifier.MatchString(after) {
		return nil, ErrInvalid
	}
	return queryOverlays(ctx, s.db, " AND id>? ORDER BY id LIMIT 101", id, after)
}

func (s *Store) AddProductionOverlay(ctx context.Context, actor Identity, id string, in OverlayRequest, font [32]byte) (Overlay, error) {
	if err := in.Validate(); err != nil {
		return Overlay{}, err
	}
	if font == ([32]byte{}) {
		return Overlay{}, &ProductionInputError{"font", "the approved font must pass preview before going on air"}
	}
	return transaction(ctx, s, func(tx *sql.Tx) (Overlay, error) {
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "editor", "director")
		if err != nil {
			return Overlay{}, err
		}
		if p.State != "draft" && p.State != "on_air" {
			return Overlay{}, ErrConflict
		}
		on, err := productionCommittedUntil(ctx, tx, p)
		if err != nil {
			return Overlay{}, err
		}
		if in.Start != nil {
			if *in.Start < on {
				return Overlay{}, &ProductionInputError{"on_frame", "choose a frame outside the rendered or reserved output"}
			}
			on = *in.Start
		}
		off := on + int64(in.Length)
		if off > ProductionMaxFrames {
			return Overlay{}, &ProductionInputError{"duration_frames", "the overlay extends past the production clock"}
		}
		var overlap bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM production_overlays WHERE production_id=? AND template=?
			AND on_frame<? AND off_frame>? AND off_frame>on_frame)`, id, in.Template, off, on).Scan(&overlap); err != nil {
			return Overlay{}, err
		}
		if overlap {
			return Overlay{}, &ProductionInputError{"on_frame", "an overlay from this template already occupies the requested interval"}
		}
		value := Overlay{ID: key(), Template: in.Template, Content: in.Content, Font: hex.EncodeToString(font[:]), Size: in.Size,
			On: on, Off: off, Planned: off, Actor: actor.Label}
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_overlays(id,production_id,template,content,font_digest,font_size,
			on_frame,off_frame,expected_off_frame,actor) VALUES(?,?,?,?,?,?,?,?,?,?)`, value.ID, id, value.Template, value.Content,
			font[:], value.Size, on, off, off, actor.Label); err != nil {
			return Overlay{}, err
		}
		return value, audit(ctx, tx, p.Output, "production_overlay", actor.Label, map[string]any{"overlay_id": value.ID, "on_frame": on, "off_frame": off})
	})
}

func (s *Store) ClearProductionOverlays(ctx context.Context, actor Identity, id string) (int64, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (int64, error) {
		p, err := authorizedProduction(ctx, tx, actor, id, true, "owner", "editor", "director")
		if err != nil {
			return 0, err
		}
		if p.State != "draft" && p.State != "on_air" {
			return 0, ErrConflict
		}
		off, err := productionCommittedUntil(ctx, tx, p)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE production_overlays SET off_frame=GREATEST(?,on_frame),cleared_by=?
			WHERE production_id=? AND off_frame>? AND cleared_by IS NULL`, off, actor.Label, p.ID, off); err != nil {
			return 0, err
		}
		return off, audit(ctx, tx, p.Output, "production_overlay_clear", actor.Label, map[string]int64{"off_frame": off})
	})
}
