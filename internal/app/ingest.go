package app

import (
	"fmt"
	"log/slog"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/types"
)

// BuildIngestJob converts an already-parsed NZB plus caller-supplied options
// into a runtime Job, resolving sanitize/category/on-demand-par2 settings
// from live config. It is a free function rather than an *Application
// method so that internal/api (which holds its own *config.Config but no
// *Application reference) can call it without new interface plumbing.
//
// It does not parse the NZB (parse failures and job-construction failures
// need to stay distinguishable at call sites, e.g. HTTP 400 vs 500) and does
// not enqueue the job via Application.AddJob (callers may need to inspect
// the constructed job, e.g. reject an empty manifest, before enqueueing).
func BuildIngestJob(cfg *config.Config, parsed *nzb.NZB, filename string, opts types.FetchOptions, logger *slog.Logger) (*queue.Job, error) {
	addOpts := queue.AddOptions{
		Filename: filename,
		Name:     opts.NzbName,
		Category: opts.Category,
		Password: opts.Password,
		PP:       opts.PP,
		Script:   opts.Script,
		Priority: opts.Priority,
		Logger:   logger,
	}
	var sOpts fsutil.SanitizeOptions
	if cfg != nil {
		cfg.WithRead(func(c *config.Config) {
			sOpts = c.Downloads.SanitizeOptions()
			addOpts.Categories = c.Categories
			addOpts.OnDemandPar2 = c.Downloads.OnDemandPar2
		})
	}

	job, err := queue.NewJob(parsed, addOpts, sOpts)
	if err != nil {
		return nil, fmt.Errorf("create job %q: %w", filename, err)
	}
	return job, nil
}
