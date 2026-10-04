package app

import (
	"context"
	"fmt"
)

// reconcileBeforeFirstTick is Application.Start's beforeFirstTick step for
// Dispatcher.StartWith. It drops every restored job that is already filed in
// history, then runs the resume sweep (resumeAllJobs, which has its own
// argument for this placement) and files the jobs the archive peek owes a
// filing (fileOwedUnwantedFailures).
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
	if err := app.resumeAllJobs(ctx); err != nil {
		return err
	}
	return app.fileOwedUnwantedFailures(ctx)
}

// fileOwedUnwantedFailures files every restored job the archive peek had
// blocked under the fail action but not yet filed (fileOwedUnwantedFailure),
// so that such a job does not run on after a restart. It runs after the
// resume sweep, whose recomputation of a job's files it should see, and before
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
