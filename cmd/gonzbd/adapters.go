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
	"github.com/hobeone/gonzbd/internal/scheduler"
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
		})
	}
	job, err := queue.NewJob(parsed, addOpts, sOpts)
	if err != nil {
		return "", fmt.Errorf("create job %q: %w", filename, err)
	}

	if err := h.app.AddJob(ctx, job, data, false); err != nil {
		return "", fmt.Errorf("add job %q: %w", filename, err)
	}
	log.Info("ingested nzb", "filename", filename, "files", len(job.Files), "bytes", job.TotalBytes, "id", job.ID)
	return job.ID, nil
}

// schedulesFromConfig builds scheduler.ScheduleSpec values from the
// config.ScheduleConfig list. ScheduleConfig has only minute/hour/dow
// fields, so the day-of-month and month fields are synthesized as "*".
// Disabled schedules are skipped.
func schedulesFromConfig(scs []config.ScheduleConfig) ([]scheduler.ScheduleSpec, error) {
	out := make([]scheduler.ScheduleSpec, 0, len(scs))
	for _, sc := range scs {
		if !sc.Enabled {
			continue
		}
		minute := fallback(sc.Minute, "*")
		hour := fallback(sc.Hour, "*")
		dow := fallback(sc.DayOfWeek, "*")
		line := fmt.Sprintf("%s %s * * %s %s", minute, hour, dow, sc.Action)
		if sc.Arguments != "" {
			line += " " + sc.Arguments
		}
		spec, err := scheduler.Parse(line)
		if err != nil {
			return nil, fmt.Errorf("schedule %q: %w", sc.Name, err)
		}
		out = append(out, spec)
	}
	return out, nil
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
