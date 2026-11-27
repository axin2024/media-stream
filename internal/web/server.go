// Package web exposes the management API, media origin, and operator workbench.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
	"github.com/axin2024/media-stream/internal/media"
)

//go:embed ui/*
var assets embed.FS

type Server struct {
	DB           *catalog.Store
	Files        *media.Files
	FFmpeg       media.FFmpeg
	Graphics     media.Graphics
	Log          *slog.Logger
	SecureCookie bool
	IngestSlots  chan struct{}
}

type authorized func(http.ResponseWriter, *http.Request, catalog.Identity)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	ui, _ := fs.Sub(assets, "ui")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(ui)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, ui, "index.html")
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.Ready(r.Context()); err != nil {
			s.Log.Warn("catalog readiness", "error", err)
			reply(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		reply(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /api/me", s.handle(s.me))
	mux.HandleFunc("POST /api/playback-cookie", s.handle(s.playbackCookie))
	mux.HandleFunc("DELETE /api/playback-cookie", s.clearPlaybackCookie)
	mux.HandleFunc("GET /api/channels", s.handle(s.channels))
	mux.HandleFunc("POST /api/channels", s.handle(s.createChannel))
	mux.HandleFunc("POST /api/channels/{channel}/sessions", s.handle(s.openSession))
	mux.HandleFunc("GET /api/sessions/{id}", s.handle(s.session))
	mux.HandleFunc("POST /api/sessions/{id}/inputs/{source}/rotate", s.handle(s.rotate))
	mux.HandleFunc("POST /api/sessions/{id}/prefer-input", s.handle(s.prefer))
	mux.HandleFunc("PUT /api/sessions/{id}/input", s.handle(s.ingest))
	mux.HandleFunc("POST /api/sessions/{id}/input-lost", s.handle(s.inputLost))
	mux.HandleFunc("POST /api/sessions/{id}/end", s.handle(s.end))
	mux.HandleFunc("GET /api/sessions/{id}/segments", s.handle(s.segments))
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handle(s.events))
	mux.HandleFunc("GET /api/sessions/{id}/quality", s.handle(s.quality))
	mux.HandleFunc("POST /api/sessions/{id}/quality", s.handle(s.postQuality))
	mux.HandleFunc("GET /api/recordings", s.handle(s.recordings))
	mux.HandleFunc("GET /api/recordings/{id}", s.handle(s.recording))
	mux.HandleFunc("POST /api/recordings/{id}/visibility", s.handle(s.visibility))
	mux.HandleFunc("GET /api/visibility-batches", s.handle(s.visibilityBatches))
	mux.HandleFunc("GET /api/visibility-batches/{key}", s.handle(s.visibilityBatch))
	mux.HandleFunc("PUT /api/visibility-batches/{key}", s.handle(s.prepareVisibilityBatch))
	mux.HandleFunc("POST /api/visibility-batches/{key}/execute", s.handle(s.executeVisibilityBatch))
	mux.HandleFunc("GET /api/recordings/{id}/captions", s.handle(s.captions))
	mux.HandleFunc("POST /api/recordings/{id}/captions/{lang}", s.handle(s.captionUpload))
	mux.HandleFunc("GET /api/recordings/{id}/edits", s.handle(s.edits))
	mux.HandleFunc("POST /api/recordings/{id}/edits", s.handle(s.createEdit))
	mux.HandleFunc("GET /api/edits/{id}", s.handle(s.edit))
	mux.HandleFunc("GET /api/ops/nodes", s.handle(s.nodes))
	mux.HandleFunc("GET /api/ops/usage", s.handle(s.usage))
	mux.HandleFunc("GET /media/{kind}/{id}/{file...}", s.handle(s.play))
	s.productionRoutes(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; media-src 'self' blob:; img-src 'self' data: blob:; worker-src 'self' blob:; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/media/") {
			w.Header().Set("Cache-Control", "private, no-store")
		}
		mux.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) string {
	scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return secret
}

func (s *Server) handle(action authorized) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := bearer(r)
		if secret == "" && strings.HasPrefix(r.URL.Path, "/media/") {
			cookie, err := r.Cookie("media_access")
			if err == nil {
				secret = cookie.Value
			}
		}
		identity, err := s.DB.Identify(r.Context(), secret)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		action(w, r, identity)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var limit *http.MaxBytesError
	var invalid *catalog.ProductionInputError
	var control *catalog.ControlConflict
	if errors.As(err, &invalid) {
		reply(w, 422, map[string]string{"code": "invalid_production_input", "field": invalid.Field, "message": invalid.Cause})
		return
	}
	if errors.As(err, &control) {
		reply(w, 409, map[string]any{"code": "control_changed", "message": control.Error(), "current": control})
		return
	}
	status, code, message := 500, "internal", "request could not be completed"
	switch {
	case errors.As(err, &limit), errors.Is(err, media.ErrTooLarge):
		status, code, message = 413, "size_limit", "request exceeds configured size"
	case errors.Is(err, catalog.ErrMissing):
		status, code, message = 404, "not_found", "object not found"
	case errors.Is(err, catalog.ErrDenied):
		status, code, message = 403, "denied", "valid credentials with the required role are needed"
	case errors.Is(err, catalog.ErrInvalid):
		status, code, message = 400, "invalid_request", "check the request fields or source media"
	case errors.Is(err, catalog.ErrFenced):
		status, code, message = 409, "fenced", "source generation or time window is no longer current"
	case errors.Is(err, catalog.ErrConflict):
		status, code, message = 409, "conflict", "request conflicts with current state"
	case errors.Is(err, catalog.ErrBackpressure):
		status, code, message = 429, "source_ahead", "wait for the session watermark to advance"
		w.Header().Set("Retry-After", "1")
	default:
		s.Log.Error("request failed", "method", r.Method, "route", r.Pattern, "error", err)
	}
	reply(w, status, map[string]string{"code": code, "message": message})
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decode[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	var value T
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return value, err
		}
		return value, catalog.ErrInvalid
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return value, catalog.ErrInvalid
	}
	return value, nil
}

func respond[T any](s *Server, w http.ResponseWriter, r *http.Request, status int, value T, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	reply(w, status, value)
}

func number(raw string) (uint64, error) {
	value, err := strconv.ParseUint(raw, 10, 63)
	if err != nil {
		return 0, catalog.ErrInvalid
	}
	return value, nil
}

func (s *Server) me(w http.ResponseWriter, _ *http.Request, caller catalog.Identity) {
	reply(w, 200, map[string]string{"tenant": caller.Tenant, "role": caller.Role, "name": caller.Label, "channel": caller.Channel})
}

func (s *Server) playbackCookie(w http.ResponseWriter, r *http.Request, caller catalog.Identity) {
	if caller.Role == "push" {
		s.fail(w, r, catalog.ErrDenied)
		return
	}
	s.setCookie(w, r, bearer(r), 3_600)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clearPlaybackCookie(w http.ResponseWriter, r *http.Request) {
	s.setCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: "media_access", Value: value, Path: "/media/", MaxAge: age, HttpOnly: true,
		Secure: s.SecureCookie || r.TLS != nil, SameSite: http.SameSiteStrictMode})
}

func (s *Server) measure(r *http.Request, session string, count int64, failed bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	defer cancel()
	if err := s.DB.Egress(ctx, session, count, failed); err != nil {
		s.Log.Warn("record media egress", "session", session, "error", err)
	}
}
