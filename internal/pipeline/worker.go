// Package pipeline runs the scheduler, transcode workers, and storage lifecycle.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
	"github.com/axin2024/media-stream/internal/media"
)

type Worker struct {
	Catalog        *catalog.Store
	Files          *media.Files
	FFmpeg         media.FFmpeg
	Log            *slog.Logger
	Node           string
	Concurrency    int
	ExcludedTracks map[string]bool
}

func (w *Worker) Run(ctx context.Context) error {
	if w.Concurrency < 1 || w.Concurrency > 32 {
		return catalog.ErrInvalid
	}
	if err := w.Catalog.CheckNode(ctx, w.Node); err != nil {
		return fmt.Errorf("register worker %q: %w", w.Node, err)
	}
	if err := w.FFmpeg.Check(ctx); err != nil {
		return err
	}
	var group sync.WaitGroup
	for range w.Concurrency {
		group.Go(func() {
			if err := w.poll(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.Log.Error("worker stopped", "error", err)
			}
		})
	}
	group.Wait()
	return ctx.Err()
}

func (w *Worker) poll(ctx context.Context) error {
	timer := time.NewTicker(200 * time.Millisecond)
	defer timer.Stop()
	for {
		err := w.Step(ctx)
		if errors.Is(err, catalog.ErrMissing) {
			return fmt.Errorf("register worker %q: %w", w.Node, err)
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, catalog.ErrFenced) {
			w.Log.Warn("worker iteration", "node", w.Node, "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (w *Worker) Step(ctx context.Context) error {
	job, err := w.Catalog.Claim(ctx, w.Node)
	if err != nil {
		return err
	}
	if job != nil {
		return w.transcode(ctx, *job)
	}
	edit, err := w.Catalog.ClaimEdit(ctx, w.Node)
	if err != nil || edit == nil {
		return err
	}
	return w.clip(ctx, *edit)
}

func withLease[T any](ctx context.Context, renew func(context.Context) error, work func(context.Context) T) (T, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTicker(8 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-timer.C:
				renewal, limit := context.WithTimeout(ctx, 3*time.Second)
				err := renew(renewal)
				limit()
				if err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	value := work(ctx)
	close(stop)
	<-done
	return value, context.Cause(ctx)
}

func (w *Worker) transcode(ctx context.Context, job catalog.Job) error {
	type rendered struct {
		output []catalog.Rendition
		report string
	}
	out, err := withLease(ctx, func(ctx context.Context) error { return w.Catalog.Renew(ctx, job) },
		func(ctx context.Context) rendered {
			ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			input, err := w.Files.Path(job.Input, true)
			if err != nil {
				w.Log.Warn("source media unreadable", "job", job.ID, "error", err)
				return rendered{report: "source media unreadable"}
			}
			dir, cleanup, err := w.Files.Workspace()
			if err != nil {
				w.Log.Warn("create transcode workspace", "job", job.ID, "error", err)
				return rendered{report: "workspace unavailable"}
			}
			defer cleanup()
			var result rendered
			var failed []string
			for _, profile := range catalog.Profiles(job.Preset) {
				if w.ExcludedTracks[profile.Track] {
					failed = append(failed, profile.Track+" unavailable on worker")
					continue
				}
				target := filepath.Join(dir, profile.Track+".ts")
				err := w.FFmpeg.Transcode(ctx, input, job, profile, target)
				if err == nil {
					err = w.FFmpeg.Probe(ctx, target)
				}
				if err == nil {
					var blob catalog.Blob
					blob, err = w.Files.SavePath(target, media.MaxFragmentBytes)
					if err == nil {
						result.output = append(result.output, catalog.Rendition{Track: profile.Track, Blob: blob})
					}
				}
				if err != nil {
					failed = append(failed, profile.Track+" encode or verification failed")
					w.Log.Warn("variant unavailable", "track", profile.Track, "job", job.ID, "error", err)
				}
			}
			result.report = strings.Join(failed, ", ")
			return result
		})
	if err != nil {
		return err
	}
	return w.Catalog.Complete(ctx, job, out.output, out.report)
}

func (w *Worker) clip(ctx context.Context, job catalog.EditJob) error {
	type rendered struct {
		output, captions catalog.Blob
		from, to         int64
		err              error
	}
	out, err := withLease(ctx, func(ctx context.Context) error { return w.Catalog.RenewEdit(ctx, job) },
		func(ctx context.Context) rendered {
			ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
			defer cancel()
			fragments, caption, err := w.Catalog.EditInputs(ctx, job)
			if err != nil {
				return rendered{err: err}
			}
			dir, cleanup, err := w.Files.Workspace()
			if err != nil {
				return rendered{err: err}
			}
			defer cleanup()
			target := filepath.Join(dir, "clip.mp4")
			from, to, err := w.FFmpeg.Clip(ctx, w.Files, dir, fragments, job.Start, job.End, target)
			if err != nil {
				return rendered{err: err}
			}
			result := rendered{from: from, to: to}
			result.output, err = w.Files.SavePath(target, 1<<30)
			if err != nil {
				return rendered{err: err}
			}
			if caption != nil {
				path, err := w.Files.Path(caption.Blob, true)
				if err != nil {
					return rendered{err: err}
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return rendered{err: err}
				}
				cues, err := media.ParseVTT(data)
				if err != nil {
					return rendered{err: err}
				}
				result.captions, err = w.Files.Save(strings.NewReader(media.VTT(cues, from, to)), 1<<20)
				if err != nil {
					return rendered{err: err}
				}
			}
			return result
		})
	if err != nil {
		return err
	}
	reason := ""
	if out.err != nil {
		w.Log.Warn("clip failed", "edit", job.ID, "error", out.err)
		reason = "source, conversion, or output verification failed"
	}
	return w.Catalog.CompleteEdit(ctx, job, out.output, out.captions, out.from, out.to, reason)
}
