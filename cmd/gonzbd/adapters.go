// Adapters that bridge the Phase 7 subsystems to the queue and to each
// other. Kept in cmd/gonzbd so the subsystems themselves stay
// dependency-free and reusable in isolation.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/types"
)

// ingestHandler satisfies both dirscanner.Handler and urlgrabber.Handler.
// It takes raw NZB bytes produced by either source and enqueues a job via
// the application orchestrator to handle duplicate detection and collisions.
type ingestHandler struct {
	app    *app.Application
	config *config.Config
	logger *slog.Logger
}

func (h *ingestHandler) HandleNZB(ctx context.Context, filename string, data []byte, opts types.FetchOptions) (string, error) {
	parsed, err := nzb.Parse(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("parse nzb %q: %w", filename, err)
	}
	log := h.logger
	addOpts := queue.AddOptions{
		Filename: filename,
		Name:     opts.NzbName,
		Category: opts.Category,
		Password: opts.Password,
		PP:       opts.PP,
		Script:   opts.Script,
		Priority: opts.Priority,
		Logger:   log,
	}
	sOpts := fsutil.SanitizeOptions{}
	if h.config != nil {
		h.config.WithRead(func(cfg *config.Config) {
			sOpts = cfg.Downloads.SanitizeOptions()
			addOpts.Categories = cfg.Categories
			addOpts.OnDemandPar2 = cfg.Downloads.OnDemandPar2
		})
	}
	job, err := queue.NewJob(parsed, addOpts, sOpts)
	if err != nil {
		return "", fmt.Errorf("create job %q: %w", filename, err)
	}

	if err := h.app.AddJob(ctx, job, data, false); err != nil {
		return "", fmt.Errorf("add job %q: %w", filename, err)
	}
	log.Info("ingested nzb", "filename", filename, "files", job.Manifest().NumFiles(), "bytes", job.Manifest().TotalBytes(), "id", job.ID)
	return job.ID, nil
}
