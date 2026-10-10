package app

import (
	"context"
	"fmt"

	"github.com/hobeone/gonzbd/internal/job"
)

// reconcileBeforeFirstTick is Application.Start's beforeFirstTick step for
// Dispatcher.StartWith. It drops every restored job that is already filed in
// history, then hydrates the jobs restored paused at Fetching
// (hydratePausedJobs) and files the jobs the archive peek owes a filing
// (fileOwedUnwantedFailures).
//
// The drop is placed before the first tick because a tick routes a restored
// job onward. A job at Repairing, Extracting or Finalizing is launched into
// post-processing, and one at Fetching that is complete is reported
// download-complete by its Fetching worker (appRunner.runFetch) and then
// assessed. A duplicate of a history entry that got that far would be
// post-processed again, and its finalize would try to file it a second time.
// Nothing ticks while this runs, and Start starts the downloader and the
// post-processor only after StartWith returns.
func (app *Application) reconcileBeforeFirstTick(ctx context.Context) error {
	if app.dispatcher != nil {
		for _, row := range app.dispatcher.List() {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("app: startup reconciliation aborted: %w", err)
			}
			app.dropJobAlreadyInHistory(ctx, row.ID)
		}
	}
	if err := app.hydratePausedJobs(ctx); err != nil {
		return err
	}
	return app.fileOwedUnwantedFailures(ctx)
}

// hydratePausedJobs hydrates, and so verifies, every job restored at Fetching
// with IntentPause. No tick hydrates a paused job until it is resumed, so
// without this mode=queue reports 0% for a job whose written articles are on
// disk. A verification fault parks the job (appResidency.Hydrate) and is not
// an error here; only a cancelled ctx stops the loop.
//
// It runs synchronously inside Application.Start, so each job's verification
// read delays Start's return and so the API's start (docs/durability-contract.md,
// Accepted limitation 1), and a job it hydrates stays resident while paused.
func (app *Application) hydratePausedJobs(ctx context.Context) error {
	if app.dispatcher == nil {
		return nil
	}
	for _, row := range app.dispatcher.List() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("app: startup hydration aborted: %w", err)
		}
		j, ok := app.dispatcher.Job(row.ID)
		if !ok || row.View.State != job.Fetching || j.Intent() != job.IntentPause {
			continue
		}
		if err := app.dispatcher.LoadProgress(ctx, row.ID); err != nil {
			app.log.Warn("could not load a paused job's progress at startup; it is loaded when resumed",
				"job", row.ID, "err", err)
		}
	}
	return nil
}

// fileOwedUnwantedFailures files every restored job the archive peek had
// blocked under the fail action but not yet filed (fileOwedUnwantedFailure),
// so that such a job does not run on after a restart. It runs after
// hydratePausedJobs, whose verification of a job's files it should see, and before
// the first tick, so a complete job is deferred to its Assessing worker and
// any other is handed over before it can launch.
func (app *Application) fileOwedUnwantedFailures(ctx context.Context) error {
	if app.dispatcher == nil {
		return nil
	}
	for _, row := range app.dispatcher.List() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("app: startup reconciliation aborted: %w", err)
		}
		j, ok := app.dispatcher.Job(row.ID)
		if !ok {
			continue
		}
		// Decided before hydrating: a paused job, which is every Blocked one
		// but the fail action's, is never loaded by this sweep.
		if _, owed := app.unwantedFilingOwed(j); !owed {
			continue
		}
		if app.residency != nil {
			_ = app.residency.Hydrate(ctx, row.ID)
		}
		app.fileOwedUnwantedFailure(j, "")
	}
	return nil
}
