package app

import (
	"context"
	"fmt"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// reconcileBeforeFirstTick is Application.Start's beforeFirstTick step for
// Dispatcher.StartWith. It drops every restored job that is already filed in
// history, then files the jobs the archive peek owes a filing
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
//
// The jobs restored paused at Fetching are not loaded here: verifyPausedJobs
// verifies them after Start has returned, so that their read-back does not
// delay the API's start.
func (app *Application) reconcileBeforeFirstTick(ctx context.Context) error {
	if app.dispatcher != nil {
		for _, row := range app.dispatcher.List() {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("app: startup reconciliation aborted: %w", err)
			}
			app.dropJobAlreadyInHistory(ctx, row.ID)
		}
	}
	return app.fileOwedUnwantedFailures(ctx)
}

// defaultPausedVerifyTimeout bounds one paused job's load in verifyPausedJobs.
// The read-back of a job's complete=0 files is what it bounds, and it is sized
// for a large job on a remote mount: a job that exceeds it reports no
// progress until it is resumed, which is the cost of a deadline too short, so
// it errs long.
const defaultPausedVerifyTimeout = 10 * time.Minute

// verifyPausedJobs hydrates, and so verifies, every job restored at Fetching
// with IntentPause, one job at a time and each under pausedVerifyTimeout. No
// tick hydrates a paused job until it is resumed, so without this mode=queue
// would report 0% for a job whose written articles are on disk until the user
// resumed it. Until a job's load lands the queue reports its header's byte
// count as remaining, as it does for any restored job before its first
// hydration; once it lands the queue reports the verified progress and a
// queue_updated event names the job. A load that does not land — its deadline
// passed, or verification faulted and parked the job (appResidency.Hydrate) —
// leaves the job reporting no progress until its resume lets the tick hydrate
// it.
//
// It runs on app.wg from Start, launched after watchCompletions, so the
// Resumed completions a load queues are consumed as they are queued, and it
// stops when ctx (app.ctx) ends: Shutdown cancels it in joinAndStop and waits
// for it there with the other goroutines on app.wg. Each job's load runs under
// a context derived from ctx, so a cancellation ends the load in flight and
// nothing is attached (appResidency.Hydrate). A job resumed, removed, or
// hydrated by a tick before its turn is skipped: LoadProgress loads only a
// registered job with no progress. A job resumed during its load is hydrated
// once: the tick's Hydrate waits for the load in flight, and starts its own
// only if that one left the job non-resident. A job removed during its load
// is removed once the load ends: RemoveJob's eviction (appResidency.Evict)
// waits for it, so the load's verdicts reach SQLite before the removal's
// reclaim deletes the job's rows, and the Resumed completions it queued find
// no job.
func (app *Application) verifyPausedJobs(ctx context.Context) {
	if app.dispatcher == nil {
		return
	}
	for _, row := range app.dispatcher.List() {
		j, ok := app.dispatcher.Job(row.ID)
		if !ok || row.View.State != job.Fetching || j.Intent() != job.IntentPause {
			continue
		}
		jctx, cancel := context.WithTimeout(ctx, app.pausedVerifyTimeout)
		err := app.dispatcher.LoadProgress(jctx, row.ID)
		cancel()
		if app.pausedVerifiedHook != nil {
			app.pausedVerifiedHook(row.ID, err)
		}
		if ctx.Err() != nil {
			app.log.Info("paused-job verification stopped; the app is stopping", "job", row.ID)
			return
		}
		if err != nil {
			app.log.Warn("could not load a paused job's progress after startup; it is loaded when the job is resumed",
				"job", row.ID, "timeout", app.pausedVerifyTimeout, "err", err)
			continue
		}
		app.emit(Event{Type: "queue_updated", NzoID: row.ID})
	}
}

// fileOwedUnwantedFailures files every restored job the archive peek had
// blocked under the fail action but not yet filed (fileOwedUnwantedFailure),
// so that such a job does not run on after a restart. It runs before the
// first tick, so a complete job is deferred to its Assessing worker and any
// other is handed over before it can launch.
//
// It needs no ordering against verifyPausedJobs: the two select disjoint
// jobs. This sweep loads a job only when unwantedFilingOwed holds, which
// requires IntentRun; verifyPausedJobs loads only a job with IntentPause; and
// no path makes a paused job owed, because Dispatcher.resume, the one writer
// of IntentRun (`git grep -n 'SetIntent(job[.]IntentRun)' -- '*.go' ':!*_test.go'`
// returns 1 line), approves a Blocked job on a user's resume and refuses it
// on any other.
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
