package web

import (
	"context"
	"net/http"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
	"github.com/axin2024/media-stream/internal/media"
)

func page[T any](values []T, cursor func(T) string) map[string]any {
	next := ""
	if len(values) > 100 {
		values = values[:100]
		next = cursor(values[99])
	}
	return map[string]any{"items": values, "next": next}
}

func (s *Server) channels(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	values, err := s.DB.Channels(r.Context(), id, r.URL.Query().Get("after"))
	respond(s, w, r, 200, page(values, func(v catalog.Channel) string { return v.Key }), err)
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	value, err := decode[catalog.Channel](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.DB.CreateChannel(r.Context(), id, value)
	respond(s, w, r, 201, out, err)
}

func (s *Server) openSession(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	input, err := decode[catalog.Opening](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.DB.OpenSession(r.Context(), id, r.PathValue("channel"), r.Header.Get("Idempotency-Key"), input)
	respond(s, w, r, 201, out, err)
}

func (s *Server) session(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	value, err := s.DB.Session(r.Context(), id, r.PathValue("id"))
	respond(s, w, r, 200, value, err)
}

func (s *Server) rotate(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	input, err := decode[catalog.Rotation](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	value, err := s.DB.RotateInput(r.Context(), id, r.PathValue("id"), r.PathValue("source"), r.Header.Get("Idempotency-Key"), input)
	respond(s, w, r, 200, value, err)
}

func (s *Server) prefer(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	input, err := decode[struct {
		Source   string `json:"source"`
		Revision uint64 `json:"revision"`
	}](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	value, err := s.DB.PreferInput(r.Context(), id, r.PathValue("id"), input.Source, input.Revision)
	respond(s, w, r, 202, value, err)
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	session := r.PathValue("id")
	maximum, err := s.DB.FormatSource(r.Context(), id, session)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	offset, err := number(r.Header.Get("X-Source-Time-Ms"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	duration, err := number(r.Header.Get("X-Source-Duration-Ms"))
	if err != nil || duration < 100 || duration > uint64(maximum+250) {
		s.fail(w, r, catalog.ErrInvalid)
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
		s.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	err = s.FFmpeg.Probe(ctx, path)
	cancel()
	if err != nil {
		s.removeUnused(blob)
		s.Log.Info("source fragment rejected", "session", session, "error", err)
		s.fail(w, r, catalog.ErrInvalid)
		return
	}
	value, err := s.DB.Ingest(r.Context(), id, session, int64(offset), int(duration), blob)
	if value.Replay || safeToDiscard(err) {
		s.removeUnused(blob)
	}
	respond(s, w, r, 200, value, err)
}

func (s *Server) removeUnused(blob catalog.Blob) {
	if err := s.Files.Remove(blob.Key); err != nil {
		s.Log.Warn("discard unreferenced media", "error", err)
	}
}

func safeToDiscard(err error) bool {
	switch err {
	case catalog.ErrInvalid, catalog.ErrDenied, catalog.ErrFenced, catalog.ErrMissing, catalog.ErrConflict, catalog.ErrBackpressure:
		return true
	default:
		return false
	}
}

func (s *Server) inputLost(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	if err := s.DB.InputLost(r.Context(), id, r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) end(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	value, err := s.DB.End(r.Context(), id, r.PathValue("id"))
	respond(s, w, r, 202, value, err)
}

func (s *Server) segments(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	stream, err := s.DB.Playback(r.Context(), id, r.PathValue("id"), false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var first uint64
	if raw := r.URL.Query().Get("from"); raw != "" {
		first, err = number(raw)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	next := min(first+100, stream.NextSeq)
	values, err := s.DB.Segments(r.Context(), stream.ID, first, next)
	respond(s, w, r, 200, map[string]any{"items": values, "next": next, "watermark": stream.NextSeq}, err)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	var after uint64
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		if after, err = number(raw); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	value, err := s.DB.Events(r.Context(), id, r.PathValue("id"), after)
	respond(s, w, r, 200, value, err)
}

func (s *Server) quality(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	value, err := s.DB.Measure(r.Context(), id, r.PathValue("id"))
	respond(s, w, r, 200, value, err)
}

func (s *Server) postQuality(w http.ResponseWriter, r *http.Request, id catalog.Identity) {
	input, err := decode[catalog.QoE](w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	replay, err := s.DB.RecordQoE(r.Context(), id, r.PathValue("id"), input)
	respond(s, w, r, 200, map[string]bool{"replay": replay}, err)
}
