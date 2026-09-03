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
	"github.com/hobeone/gonzbd/internal/nzb"
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

func (h *ingestHandler) HandleNZB(ctx context.Context, filename string, data []byte, opts types.FetchOptions) (string, error) { //nocover: thin delegator to unit-tested app.BuildIngestJob, covered end-to-end by test/integration/api_test.go
	parsed, err := nzb.Parse(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("parse nzb %q: %w", filename, err)
	}
	log := h.logger
	job, hdr, err := app.BuildIngestJob(h.config, parsed, filename, opts, log)
	if err != nil {
		return "", err
	}

	if err := h.app.AddJob(ctx, job, hdr, data, false); err != nil {
		return "", fmt.Errorf("add job %q: %w", filename, err)
	}
	log.Info("ingested nzb", "filename", filename, "files", job.NumFiles(), "bytes", job.TotalBytes(), "id", job.ID())
	return job.ID(), nil
}
