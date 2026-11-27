package web

import (
	"context"
	"errors"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
	"github.com/axin2024/media-stream/internal/media"
)

func (s *Server) productionRoutes(mux *http.ServeMux) {
	const base = "/api/productions"
	const production = base + "/{production}"
	const slot = production + "/slots/{slot}"
	mux.HandleFunc("GET "+base, s.handle(s.listProductions))
	mux.HandleFunc("POST "+base, s.handle(s.createProduction))
	mux.HandleFunc("GET "+production, s.handle(s.production))
	mux.HandleFunc("GET "+production+"/slots", s.handle(s.productionSlots))
	mux.HandleFunc("PUT "+slot, s.handle(s.productionSlot))
	mux.HandleFunc("PUT "+slot+"/clock", s.handle(s.productionClock))
	mux.HandleFunc("PUT "+slot+"/input/{frame}", s.handle(s.productionInput))
	mux.HandleFunc("POST "+slot+"/lost", s.handle(s.productionInputLost))
	mux.HandleFunc("GET "+slot+"/monitor.mp4", s.handle(s.productionMonitor))
	mux.HandleFunc("GET "+production+"/rundown", s.handle(s.productionRundown))
	mux.HandleFunc("PUT "+production+"/rundown", s.handle(s.changeProductionRundown))
	mux.HandleFunc("POST "+production+"/control", s.handle(s.productionControl))
	mux.HandleFunc("POST "+production+"/preview", s.handle(s.previewProduction))
	mux.HandleFunc("POST "+production+"/start", s.handle(s.startProduction))
	mux.HandleFunc("POST "+production+"/end", s.handle(s.endProduction))
	mux.HandleFunc("POST "+production+"/takes", s.handle(s.takeProduction))
	mux.HandleFunc("GET "+production+"/takes/{take}", s.handle(s.productionTake))
	mux.HandleFunc("GET "+production+"/overlays", s.handle(s.productionOverlays))
	mux.HandleFunc("POST "+production+"/overlays/preview.png", s.handle(s.previewProductionOverlay))
	mux.HandleFunc("POST "+production+"/overlays", s.handle(s.addProductionOverlay))
	mux.HandleFunc("POST "+production+"/overlays/clear", s.handle(s.clearProductionOverlays))
	mux.HandleFunc("GET "+production+"/as-run", s.handle(s.productionAsRun))
	mux.HandleFunc("GET "+production+"/archive", s.handle(s.productionArchive))
	mux.HandleFunc("GET /media/archive/{production}/index.m3u8", s.handle(s.productionArchive))
}

func (s *Server) listProductions(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	list, err := s.DB.Productions(r.Context(), actor, r.URL.Query().Get("after"))
	respond(s, w, r, 200, page(list, func(p catalog.Production) string { return p.ID }), err)
}

func (s *Server) createProduction(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	in, err := decode[catalog.ProductionOpening](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.DB.CreateProduction(r.Context(), actor, r.Header.Get("Idempotency-Key"), in)
	respond(s, w, r, 201, p, err)
}

func (s *Server) production(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	p, err := s.DB.Production(r.Context(), actor, r.PathValue("production"))
	respond(s, w, r, 200, p, err)
}

func (s *Server) productionSlots(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	values, err := s.DB.ProductionSlots(r.Context(), actor, r.PathValue("production"))
	respond(s, w, r, 200, values, err)
}

func (s *Server) productionSlot(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	in, err := decode[catalog.SlotSettings](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	slot, err := s.DB.SetProductionSlot(r.Context(), actor, r.PathValue("production"), r.PathValue("slot"), in)
	respond(s, w, r, 200, slot, err)
}

func (s *Server) productionClock(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	in, err := decode[catalog.SlotClock](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	slot, err := s.DB.CalibrateProductionSlot(r.Context(), actor, r.PathValue("production"), r.PathValue("slot"), in)
	respond(s, w, r, 200, slot, err)
}

func (s *Server) productionInput(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	id, slot := r.PathValue("production"), r.PathValue("slot")
	source, err := s.DB.ProductionInputAllowed(r.Context(), actor, id, slot)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	frame, frameErr := strconv.ParseInt(r.PathValue("frame"), 10, 64)
	count, countErr := strconv.Atoi(r.Header.Get("X-Frame-Count"))
	if source.ZeroMS == nil || frameErr != nil || countErr != nil || frame < 0 || frame > catalog.ProductionMaxFrames*2 || count < 1 || count > 250 {
		s.fail(w, r, &catalog.ProductionInputError{Field: "source_frame", Cause: "a calibrated slot and 1 to 250 sequential source frames are required"})
		return
	}
	select {
	case s.IngestSlots <- struct{}{}:
		defer func() { <-s.IngestSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		reply(w, 503, map[string]string{"code": "input_busy", "message": "retry after the input verifier has capacity"})
		return
	}
	blob, err := s.Files.Save(http.MaxBytesReader(w, r.Body, media.MaxFragmentBytes), media.MaxFragmentBytes)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path, err := s.Files.Path(blob, false)
	if err != nil {
		s.removeUnused(blob)
		s.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	err = s.FFmpeg.VerifyProductionInput(ctx, path, count, source.Audio)
	cancel()
	if err != nil {
		s.removeUnused(blob)
		s.Log.Info("production input verification", "production", id, "slot", slot, "error", err)
		s.fail(w, r, &catalog.ProductionInputError{Field: "fragment", Cause: "expected an independent 25 fps H.264 MPEG-TS fragment with the declared frames and audio channel"})
		return
	}
	accepted, err := s.DB.PutProductionInput(r.Context(), actor, id, slot, frame, count, blob)
	var invalid *catalog.ProductionInputError
	if accepted.Replay || safeToDiscard(err) || errors.As(err, &invalid) {
		s.removeUnused(blob)
	}
	respond(s, w, r, 200, accepted, err)
}

func (s *Server) productionInputLost(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	if err := s.DB.MarkProductionInputLost(r.Context(), actor, r.PathValue("production"), r.PathValue("slot")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) productionMonitor(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	blob, err := s.DB.ProductionPreviewMedia(r.Context(), actor, r.PathValue("production"), r.PathValue("slot"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path, err := s.Files.Path(blob, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	clip, err := s.FFmpeg.ProductionPreviewClip(ctx, path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	_, _ = io.Copy(w, clip)
}

func (s *Server) productionRundown(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	plan, err := s.DB.Rundown(r.Context(), actor, r.PathValue("production"))
	respond(s, w, r, 200, plan, err)
}

func (s *Server) changeProductionRundown(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	in, err := decode[catalog.RundownChange](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	plan, err := s.DB.ChangeRundown(r.Context(), actor, r.PathValue("production"), in)
	status := 200
	if len(plan.Conflict) != 0 {
		status = 409
	}
	respond(s, w, r, status, plan, err)
}

func (s *Server) productionControl(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	in, err := decode[catalog.ControlRequest](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.DB.ClaimProductionControl(r.Context(), actor, r.PathValue("production"), in)
	respond(s, w, r, 200, p, err)
}

type productionCommand struct {
	Slot  string `json:"slot_id,omitempty"`
	Epoch uint64 `json:"expected_control_epoch"`
}

func (s *Server) previewProduction(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	in, err := decode[productionCommand](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.DB.SetProductionPreview(r.Context(), actor, r.PathValue("production"), in.Slot, in.Epoch)
	respond(s, w, r, 200, p, err)
}

func (s *Server) startProduction(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	in, err := decode[productionCommand](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.DB.StartProduction(r.Context(), actor, r.PathValue("production"), in.Epoch)
	respond(s, w, r, 202, p, err)
}

func (s *Server) endProduction(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	in, err := decode[productionCommand](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.DB.EndProduction(r.Context(), actor, r.PathValue("production"), in.Epoch)
	respond(s, w, r, 202, p, err)
}

func (s *Server) takeProduction(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	in, err := decode[catalog.TakeRequest](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	take, err := s.DB.TakeProduction(r.Context(), actor, r.PathValue("production"), r.Header.Get("Idempotency-Key"), in)
	status := 200
	if err == nil && take.State == "queued" {
		status = 202
	}
	if err == nil {
		w.Header().Set("Location", r.URL.Path+"/"+take.ID)
	}
	respond(s, w, r, status, take, err)
}

func (s *Server) productionTake(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	take, err := s.DB.ProductionTake(r.Context(), actor, r.PathValue("production"), r.PathValue("take"))
	respond(s, w, r, 200, take, err)
}

func (s *Server) productionOverlays(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	values, err := s.DB.ProductionOverlays(r.Context(), actor, r.PathValue("production"), r.URL.Query().Get("after"))
	respond(s, w, r, 200, page(values, func(o catalog.Overlay) string { return o.ID }), err)
}

func (s *Server) overlayAccess(r *http.Request, actor catalog.Identity) error {
	p, err := s.DB.Production(r.Context(), actor, r.PathValue("production"))
	if err != nil {
		return err
	}
	if !actor.Allows(p.Tenant, p.Channel, p.Output, "owner", "director", "editor") {
		return catalog.ErrMissing
	}
	return nil
}

func (s *Server) previewProductionOverlay(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	if err := s.overlayAccess(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	in, err := decode[catalog.OverlayRequest](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	graphic, _, err := s.Graphics.Preview(in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_ = png.Encode(w, graphic)
}

func (s *Server) addProductionOverlay(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	if err := s.overlayAccess(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	in, err := decode[catalog.OverlayRequest](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	_, font, err := s.Graphics.Preview(in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	created, err := s.DB.AddProductionOverlay(r.Context(), actor, r.PathValue("production"), in, font)
	respond(s, w, r, 201, created, err)
}

func (s *Server) clearProductionOverlays(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	frame, err := s.DB.ClearProductionOverlays(r.Context(), actor, r.PathValue("production"))
	respond(s, w, r, 200, map[string]int64{"off_frame": frame}, err)
}

func (s *Server) productionAsRun(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	var after int64
	var err error
	if raw := r.URL.Query().Get("from_frame"); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
	}
	if err != nil {
		s.fail(w, r, catalog.ErrInvalid)
		return
	}
	p, entries, err := s.DB.ProductionAsRun(r.Context(), actor, r.PathValue("production"), after, 101)
	var next *int64
	if len(entries) > 100 {
		next = &entries[100].Start
		entries = entries[:100]
	}
	respond(s, w, r, 200, map[string]any{"asrun_rev": p.AsRunRev, "program_frame": p.Frame, "state": p.State, "items": entries, "next_frame": next}, err)
}
