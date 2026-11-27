package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
	"github.com/axin2024/media-stream/internal/media"
)

type ProductionRenderer struct {
	Catalog  *catalog.Store
	Files    *media.Files
	FFmpeg   media.FFmpeg
	Graphics media.Graphics
	Log      *slog.Logger
}

func (r ProductionRenderer) Run(ctx context.Context) error {
	tick := time.NewTicker(125 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := r.Step(ctx); err != nil && ctx.Err() == nil {
				r.Log.Warn("production renderer", "error", err)
			}
		}
	}
}

func (r ProductionRenderer) Step(ctx context.Context) error {
	var after string
	for {
		ids, err := r.Catalog.ActiveProductions(ctx, after)
		if err != nil || len(ids) == 0 {
			return err
		}
		for _, id := range ids {
			if err := r.render(ctx, id); err != nil && ctx.Err() == nil {
				r.Log.Warn("render production interval", "production", id, "error", err)
			}
		}
		after = ids[len(ids)-1]
	}
}

func (r ProductionRenderer) render(ctx context.Context, id string) error {
	job, err := r.Catalog.ClaimProductionFrames(ctx, id)
	if err != nil || job.Claim == "" {
		return err
	}
	encode, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	out, err := r.FFmpeg.RenderProduction(encode, r.Files, r.Graphics, job)
	if err != nil {
		if ctx.Err() == nil {
			err = errors.Join(err, r.Catalog.ReportProductionRenderFailure(ctx, job, "media_render_failed"))
		}
		return err
	}
	if err := r.Catalog.PublishProductionFrames(ctx, job, out); err != nil {
		if errors.Is(err, catalog.ErrFenced) || errors.Is(err, catalog.ErrInvalid) {
			for _, track := range out {
				_ = r.Files.Remove(track.Blob.Key)
			}
			if errors.Is(err, catalog.ErrFenced) {
				return nil
			}
		}
		// A transport error during commit has an uncertain outcome. Reconciliation handles unreferenced files.
		return errors.Join(err, r.Catalog.ReportProductionRenderFailure(ctx, job, "publication_failed"))
	}
	return nil
}
