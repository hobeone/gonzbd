package app

import (
	"context"
	"log/slog"

	"github.com/hobeone/gonzbd/internal/job"
)

// reporter is how a runner tells the dispatcher a job's work ended. It is an
// interface so the exactly-once test can observe the calls; production passes
// the Dispatcher itself.
type reporter interface {
	Finished(id string, o job.Outcome) error
	Yielded(id string) error
}

// appRunner routes a job at one state to the subsystem that does that state's
// work, and returns immediately.
//
// Every branch must end in exactly one Finished or Yielded, on some goroutine.
// Returning without either strands the job's lease and compute slot: the Queue
// cannot distinguish "holding and working" from "holding and yielded", so
// nothing else can return them (ports.go, Runner).
type appRunner struct {
	app    *Application
	report reporter
	log    *slog.Logger
}

func newAppRunner(app *Application) *appRunner {
	var l *slog.Logger
	if app != nil && app.log != nil {
		l = app.log
	} else {
		l = slog.New(slog.DiscardHandler)
	}
	return &appRunner{app: app, log: l}
}

func (r *appRunner) Run(ctx context.Context, id string, state job.State) {
	switch state {
	case job.Fetching:
		go r.runFetch(ctx, id)
	case job.Assessing:
		go r.runAssess(ctx, id)
	case job.Repairing, job.Extracting, job.Finalizing:
		go r.runPostProc(ctx, id, state)
	default:
		// A state with no work is still a state the dispatcher leased. Yield
		// rather than return silently, or the lease is never released.
		r.log.Warn("runner: no work for state; yielding", "job", id, "state", state)
		if r.report != nil {
			if err := r.report.Yielded(id); err != nil {
				r.log.Error("runner: yield failed", "job", id, "error", err)
			}
		}
	}
}

func (r *appRunner) runFetch(_ context.Context, id string) {
	if r.report != nil {
		_ = r.report.Yielded(id)
	}
}

func (r *appRunner) runAssess(_ context.Context, id string) {
	if r.report != nil {
		_ = r.report.Yielded(id)
	}
}

func (r *appRunner) runPostProc(_ context.Context, id string, _ job.State) {
	if r.report != nil {
		_ = r.report.Yielded(id)
	}
}
