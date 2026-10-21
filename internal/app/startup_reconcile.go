package app

import (
	"context"
	"fmt"
)

// reconcileBeforeFirstTick is Application.Start's beforeFirstTick step for
// Dispatcher.StartWith. It drops every restored job that is already filed in
// history, then attaches each remaining job's file table and seeded progress
// (restoreProgress), then files the jobs owed an unwanted-extension filing
// (fileOwedUnwantedFailures), whose hydration needs the table.
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
	for _, row := range app.dispatcher.List() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("app: startup reconciliation aborted: %w", err)
		}
		app.dropJobAlreadyInHistory(ctx, row.ID)
	}
	if err := app.restoreProgress(ctx); err != nil {
		return err
	}
	return app.fileOwedUnwantedFailures(ctx)
}

// fileOwedUnwantedFailures files every restored job the fail action blocked
// but did not file (unwanted.StateFailOwed; fileOwedUnwantedFailure), so that
// such a job does not run on after a restart. It runs before the first tick,
// so a resident complete job is deferred to its Assessing worker and any
// other, including one that could not be hydrated, is handed over before it
// can launch.
func (app *Application) fileOwedUnwantedFailures(ctx context.Context) error {
	for _, row := range app.dispatcher.List() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("app: startup reconciliation aborted: %w", err)
		}
		j, ok := app.dispatcher.Job(row.ID)
		if !ok {
			continue
		}
		// Decided before hydrating: a job the pause action blocked is never
		// loaded by this sweep.
		if _, owed := app.unwantedFilingOwed(j); !owed {
			continue
		}
		// A failed hydration does not stop the filing: a job is owed by its
		// unwanted state alone, which a hydration's storage fault does not
		// change even though it pauses the job (parkVerifyFault). Such a job,
		// or one whose manifest is unreadable, is filed without its verified
		// files, and at once: a job that is not resident has no Assessing
		// worker to take a deferred reason, however complete the restore
		// pass's seeded flags read (fileOwedUnwantedFailure).
		if app.residency != nil {
			if err := app.residency.Hydrate(ctx, row.ID); err != nil {
				app.log.Warn("startup: loading a job owed an unwanted-extension filing failed; it is filed without its verified files",
					"job", row.ID, "err", err)
			}
		}
		app.fileOwedUnwantedFailure(j, "")
	}
	return nil
}
