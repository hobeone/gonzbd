package app

import (
	"context"
	"fmt"
)

// reconcileBeforeFirstTick is Application.Start's beforeFirstTick step for
// Dispatcher.StartWith. It drops every restored job that is already filed in
// history, then runs the resume sweep (resumeAllJobs, which has its own
// argument for this placement).
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
	return app.resumeAllJobs(ctx)
}
