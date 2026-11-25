package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
	"github.com/axin2024/media-stream/internal/media"
)

type Scheduler struct {
	Catalog *catalog.Store
	Files   *media.Files
	Log     *slog.Logger
	Grace   time.Duration
}

func (s Scheduler) Run(ctx context.Context) error {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	maintenance := time.NewTicker(15 * time.Second)
	defer maintenance.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := s.Step(ctx); err != nil && ctx.Err() == nil {
				s.Log.Warn("schedule sessions", "error", err)
			}
		case <-maintenance.C:
			if err := s.Collect(ctx); err != nil && ctx.Err() == nil {
				s.Log.Warn("collect media", "error", err)
			}
		}
	}
}

func (s Scheduler) Step(ctx context.Context) error {
	var after string
	for {
		ids, err := s.Catalog.PendingSessions(ctx, after)
		if err != nil || len(ids) == 0 {
			return err
		}
		for _, id := range ids {
			if err := s.Catalog.Schedule(ctx, id); err != nil && ctx.Err() == nil {
				s.Log.Warn("schedule session", "session", id, "error", err)
			}
		}
		after = ids[len(ids)-1]
	}
}

func (s Scheduler) Collect(ctx context.Context) error {
	if err := s.Catalog.ExpirePublications(ctx); err != nil {
		return err
	}
	if _, err := s.Catalog.PurgeHidden(ctx, s.Grace); err != nil {
		return err
	}
	keys, err := s.Catalog.Collectible(ctx, s.Grace)
	if err != nil {
		return err
	}
	var failed error
	for _, key := range keys {
		if err := s.Files.Remove(key); err != nil {
			failed = errors.Join(failed, err)
			continue
		}
		failed = errors.Join(failed, s.Catalog.MarkDeleted(ctx, key))
	}
	return failed
}

func (s Scheduler) SweepOrphans(ctx context.Context) error {
	return s.Files.Orphans(time.Now().Add(-48*time.Hour), func(key string, incomplete bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if incomplete {
			return s.Files.RemoveIncomplete(key)
		}
		found, err := s.Catalog.KnownBlob(ctx, key)
		if err != nil || found {
			return err
		}
		return s.Files.Remove(key)
	})
}
