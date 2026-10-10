package app

import (
	"context"
	"errors"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// Dispatcher returns the application's job dispatcher.
func (app *Application) Dispatcher() *dispatch.Dispatcher {
	return app.dispatcher
}

// Config returns the application's configuration.
func (app *Application) Config() *config.Config {
	return app.config
}

func (app *Application) lookupJob(id string) (*job.Job, bool) {
	if app.dispatcher != nil {
		return app.dispatcher.Job(id)
	}
	return nil, false
}

// appWorkers satisfies sched.Workers.
type appWorkers struct {
	app *Application
}

// Abort calls CancelJob on the downloader pool before yielding the job on the
// dispatcher. CancelJob reaps the downloader's tracking records (tryList and
// inFlight): those under the job's ID, and those of every instance no longer
// registered under its own ID. It does not cancel or drain active NNTP worker
// goroutines. It reads the registry through Dispatcher.Job, so Abort also
// takes Dispatcher.mu, which D-B9 forbids holding across a call into Queue
// (internal/dispatch/tick.go), so it keeps the lock rule below.
//
// It does not yield a job the post-processor holds, queued or running. The
// question is asked of this instance (HasJob), not of the job's ID: the
// post-processor releases only the instance it holds, so an earlier instance
// still there says nothing about who releases this one's claim. A
// running stage stops only once it next checks its context, and the launch
// claim is what holds RemoveJob back from the job's files. The claim is
// released instead once the post-processor lets the job go:
// jobFinalizer.cancelled (OnJobCancelled) for a job its CancelJob took,
// persistAndCommit (OnJobDone) for one it finished, and, for one a stop
// dropped, Shutdown's yield of every Repairing, Extracting or Finalizing
// row. Shutdown yields only when PostProcessor.Stop returned within its step
// timeout; otherwise nothing releases the claim, and Dispatcher.Stop gives up
// waiting on it after its per-job timeout, as it does for any worker still
// running.
//
// pp.HasJob takes q.mu and busyMu, and no span of either calls out of
// internal/postproc, so it keeps the lock rule sched.Workers places on Abort.
// `git grep -n 'q\.mu\.Lock()\|busyMu\.Lock()' -- internal/postproc/postproc.go internal/postproc/queue.go internal/postproc/has.go internal/postproc/cancel.go`
// returns 12 lines, the spans that claim covers.
func (w *appWorkers) Abort(j *job.Job) {
	if w.app == nil || j == nil {
		return
	}
	jobID := j.ID()
	w.app.mu.Lock()
	dl := w.app.downloader
	disp := w.app.dispatcher
	log := w.app.log
	w.app.mu.Unlock()
	pp := w.app.postProcessor

	if dl != nil {
		if c, ok := dl.(interface{ CancelJob(string) }); ok {
			c.CancelJob(jobID)
		}
		if wk, ok := dl.(interface{ Wake() }); ok {
			wk.Wake()
		}
	}
	if pp != nil && pp.HasJob(j) {
		return
	}
	if disp != nil {
		// Yield asynchronously: Abort is invoked inside sched.Queue.mu's lock span
		// during Cancel. Calling disp.YieldedJob directly would deadlock on Queue.mu.
		// If the job was already removed via Dispatcher.Remove or replaced by a new
		// attempt under the same ID, disp.YieldedJob returns an ErrNotFound which is
		// benign and logged at debug level.
		go func() {
			if err := disp.YieldedJob(j); err != nil && log != nil {
				if errors.Is(err, dispatch.ErrNotFound) {
					log.Debug("abort: worker yield completed with notice", "job", jobID, "err", err)
				} else {
					log.Warn("abort: worker yield failed", "job", jobID, "err", err)
				}
			}
		}()
	}
}

type nopDispatchStore struct{}

func (nopDispatchStore) Load(context.Context) ([]dispatch.Persisted, error) { return nil, nil }
func (nopDispatchStore) Save(context.Context, dispatch.Persisted) error     { return nil }
func (nopDispatchStore) Delete(context.Context, string) error               { return nil }
