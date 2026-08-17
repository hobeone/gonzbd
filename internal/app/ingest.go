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
		snap := cfg.IngestSnapshot()
		sOpts = snap.Downloads.SanitizeOptions()
		addOpts.Categories = snap.Categories
		addOpts.OnDemandPar2 = snap.Downloads.OnDemandPar2
	}

	logParseAnomalies(logger, filename, parsed)

	job, err := queue.NewJob(parsed, addOpts, sOpts)
	if err != nil {
		return nil, fmt.Errorf("create job %q: %w", filename, err)
	}
	return job, nil
}

// logParseAnomalies reports what the parser discarded, once per ingest.
//
// The nzb package counts these and returns them; until now nothing read them,
// so an NZB could lose segments to a size check or a repeated Message-ID and
// say so to no one. The job then downloads short and the user learns about it
// from par2, or from a file that never completes.
//
// This is the only place all four nzb.Parse call sites converge, and the only
// one holding both the counters and a logger — the parser deliberately has
// neither a logger nor any knowledge of who called it.
//
// A repeated Message-ID is called out separately from the other counters
// because it is the one whose absence downstream code is entitled to assume:
// dropping it is what makes Message-ID a unique key within a job.
func logParseAnomalies(logger *slog.Logger, filename string, parsed *nzb.NZB) {
	if logger == nil || parsed == nil {
		return
	}
	if parsed.DuplicateMessageIDs == 0 && parsed.DuplicateArticles == 0 &&
		parsed.BadArticles == 0 && parsed.SkippedFiles == 0 {
		return
	}
	logger.Warn("NZB contains malformed segments; they were discarded at ingest",
		"filename", filename,
		"duplicate_message_ids", parsed.DuplicateMessageIDs,
		"duplicate_part_numbers", parsed.DuplicateArticles,
		"implausible_size", parsed.BadArticles,
		"files_without_usable_segments", parsed.SkippedFiles,
	)
}
