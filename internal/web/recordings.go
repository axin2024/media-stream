package web

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
	"github.com/axin2024/media-stream/internal/media"
)

func (s *Server) recordings(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	values, err := s.DB.Recordings(r.Context(), id, r.URL.Query().Get("before"))
	respond(s, w, r, 200, page(values, func(v catalog.Recording) string { return v.ID }), err)
}

func (s *Server) recording(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	value, err := s.DB.Recording(r.Context(), id, r.PathValue("id"))
	respond(s, w, r, 200, value, err)
}

func (s *Server) visibility(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	input, err := decode[struct {
		Revision uint64     `json:"revision"`
		Until    *time.Time `json:"visible_until"`
		Hide     bool       `json:"hide"`
	}](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	value, err := s.DB.SetVisibility(r.Context(), id, r.PathValue("id"), input.Revision, input.Until, input.Hide)
	respond(s, w, r, 200, value, err)
}

func (s *Server) visibilityBatches(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	var before uint64
	if raw := r.URL.Query().Get("before"); raw != "" {
		var err error
		before, err = number(raw)
		if err != nil {
			s.fail(w, r, err)
			return
		}
	}
	items, err := s.DB.VisibilityBatches(r.Context(), id, before)
	var next string
	if len(items) > 25 {
		items = items[:25]
		next = strconv.FormatUint(items[len(items)-1].ID, 10)
	}
	respond(s, w, r, 200, map[string]any{"items": items, "next": next}, err)
}

func (s *Server) visibilityBatch(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	value, err := s.DB.VisibilityBatch(r.Context(), id, r.PathValue("key"))
	respond(s, w, r, 200, value, err)
}

func (s *Server) prepareVisibilityBatch(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	input, err := decode[catalog.VisibilityBatchRequest](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	value, err := s.DB.PrepareVisibilityBatch(r.Context(), id, r.PathValue("key"), input)
	respond(s, w, r, 200, value, err)
}

func (s *Server) executeVisibilityBatch(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	value, err := s.DB.ExecuteVisibilityBatch(r.Context(), id, r.PathValue("key"))
	respond(s, w, r, 200, value, err)
}

func (s *Server) captions(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	value, err := s.DB.Captions(r.Context(), id, r.PathValue("id"))
	respond(s, w, r, 200, value, err)
}

func (s *Server) captionUpload(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	if id.Role != "editor" && id.Role != "owner" {
		s.fail(w, r, catalog.ErrDenied)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := media.ParseVTT(data); err != nil {
		s.fail(w, r, catalog.ErrInvalid)
		return
	}
	blob, err := s.Files.Save(bytes.NewReader(data), 1<<20)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	value, err := s.DB.AddCaption(r.Context(), id, r.PathValue("id"), r.PathValue("lang"), r.Header.Get("Idempotency-Key"), blob)
	if (err == nil && value.Blob.Key != blob.Key) || safeToDiscard(err) {
		s.removeUnused(blob)
	}
	respond(s, w, r, 201, value, err)
}

func (s *Server) edits(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	values, err := s.DB.Edits(r.Context(), id, r.PathValue("id"), r.URL.Query().Get("after"))
	respond(s, w, r, 200, page(values, func(v catalog.Edit) string { return v.ID }), err)
}

func (s *Server) edit(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	value, err := s.DB.Edit(r.Context(), id, r.PathValue("id"))
	respond(s, w, r, 200, value, err)
}

func (s *Server) createEdit(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	input, err := decode[catalog.EditRequest](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	value, err := s.DB.CreateEdit(r.Context(), id, r.PathValue("id"), r.Header.Get("Idempotency-Key"), input)
	respond(s, w, r, 202, value, err)
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	value, err := s.DB.Nodes(r.Context(), id)
	respond(s, w, r, 200, value, err)
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		var err error
		since, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			s.fail(w, r, catalog.ErrInvalid)
			return
		}
	}
	value, err := s.DB.Usage(r.Context(), id, since)
	respond(s, w, r, 200, value, err)
}
