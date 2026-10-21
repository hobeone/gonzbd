package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// restoreProgress attaches a FileTable and a seeded JobProgress to every
// registered job, from one read of job_files and the written_articles
// counts. Runs inside reconcileBeforeFirstTick, after dropJobAlreadyInHistory
// and before fileOwedUnwantedFailures. Seeds no article bit
// (docs/durability-contract.md §3).
//
// The seed (seedOf) leaves every file unverified, so each job's first
// hydration in this process verifies it (appResidency.Hydrate).
//
// A job whose structure cannot be read — no job_files rows, or rows that do
// not cover its file indices once each — cannot be shown or run. It is given
// an operational error naming the gap and settled Failed through
// Dispatcher.SettleFailed, the door the tick settles an unreadable manifest
// through, and it is left without a table. A job with no open attempt (one
// that never ran) is refused by that door; its first hydration then fails for
// want of a table, and the tick settles it Failed there. A read of the record
// that fails as a whole fails Start.
//
// A table already attached is an error naming the job. Ingest attaches one,
// and cmd/gonzbd/main.go calls Start before it can ingest: at :246, before
// the ingest handler is built (:265) and the API served, and at :1030, before
// the one-shot AddJob (:1048). So a registered job reaching this pass was
// restored, and a restored job has none.
func (app *Application) restoreProgress(ctx context.Context) error {
	if app.durable == nil {
		return nil
	}
	rows, err := app.durable.SeedRows(ctx)
	if err != nil {
		return fmt.Errorf("app: restore progress: %w", err)
	}
	for _, row := range app.dispatcher.List() {
		j, ok := app.dispatcher.Job(row.ID)
		if !ok {
			// Removed since List: nothing left to restore.
			app.log.Debug("restore progress: job removed before its turn", "job", row.ID)
			continue
		}
		if j.Files() != nil {
			return fmt.Errorf("app: restore progress: job %s already has a file table before the restore pass", row.ID)
		}
		seeds := rows[row.ID]
		ft, err := fileTableFromRows(toFileRows(seeds))
		if err == nil && len(seeds) == 0 {
			err = errors.New("no job_files rows")
		}
		if err == nil {
			err = j.AttachFiles(ft, seedOf(seeds))
		}
		if err != nil {
			app.failUnrestorable(j, err)
		}
	}
	return nil
}

// failUnrestorable records why j's structure could not be restored and
// settles it Failed (restoreProgress).
func (app *Application) failUnrestorable(j *job.Job, cause error) {
	msg := "the job's file structure could not be restored from job_files: " + cause.Error()
	app.log.Warn("restore progress: settling a job whose structure cannot be read Failed", "job", j.ID(), "err", cause)
	warnUnlessGone(app.log, "record the operational error", j.ID(), app.dispatcher.SetOperationalError(j.ID(), msg))
	if err := app.dispatcher.SettleFailed(j); err != nil {
		app.log.Warn("restore progress: could not settle the job now; its first hydration settles it", "job", j.ID(), "err", err)
	}
}

// toFileRows projects the restore read's rows onto the per-job row type the
// file-table build takes.
func toFileRows(rows []durability.SeedRow) []durability.FileRow {
	out := make([]durability.FileRow, len(rows))
	for i, r := range rows {
		out[i] = r.FileRow
	}
	return out
}

// seedOf is the per-file seed AttachFiles takes, from rows in file-index
// order (SeedRows orders them, and fileTableFromRows has refused any that do
// not cover the indices once each). It carries what the record says of each
// file and no article bit.
func seedOf(rows []durability.SeedRow) []job.FileSeed {
	seed := make([]job.FileSeed, len(rows))
	for i, r := range rows {
		seed[i] = job.FileSeed{
			Recorded: r.Recorded,
			Complete: r.Complete,
			Filename: r.Filename,
			Fetch:    job.FetchPolicy(r.FetchPolicy),
		}
	}
	return seed
}

// fileTableFromRows builds the job's file table from its job_files rows. The
// rows must cover file indices 0..n-1 exactly once each; a gap or a duplicate
// is an error naming the index, because a table built over them would index
// every later file's articles against the wrong range. No rows is a table of
// no files.
func fileTableFromRows(rows []durability.FileRow) (*job.FileTable, error) {
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a, b durability.FileRow) int { return cmp.Compare(a.FileIndex, b.FileIndex) })
	specs := make([]job.FileSpec, len(sorted))
	for i, row := range sorted {
		if row.FileIndex != i {
			return nil, fmt.Errorf("job_files rows do not cover file indices 0..%d once each: expected index %d, found %d",
				len(sorted)-1, i, row.FileIndex)
		}
		specs[i] = job.FileSpec{Subject: row.Subject, Bytes: row.Bytes, ArticleCount: row.ArticleCount}
	}
	return job.NewFileTable(specs), nil
}
