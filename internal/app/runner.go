package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// reporter is how a runner tells the dispatcher a job's work ended. It is an
// interface so the exactly-once test can observe the calls; production passes
// the Dispatcher itself.
type reporter interface {
	FinishedJob(j *job.Job, o job.Outcome) error
	Yielded(id string) error
	AdvanceFrom(j *job.Job, from, next job.State) error
}

// appRunner routes a job at one state to the subsystem that does that state's
// work, and returns immediately.
//
// runAssess executes work synchronously in-goroutine and passes ctx to
// maybeReleaseRecoveryVolumes(ctx, j). runFetch and runPostProc hand off work
// to downstream subsystem pools (downloader and postProcessor), whose cancellation
// and worker drains are owned by their respective Stop methods during
// Application.Shutdown.
//
// Every branch must end in exactly one FinishedJob, Yielded or AdvanceFrom, on
// some goroutine.
// Returning without one strands the job's lease and compute slot: the Queue
// cannot distinguish "holding and working" from "holding and yielded", so
// nothing else can return them (ports.go, Runner).
//
// The obligation is discharged either synchronously within the runner or via
// four documented downstream pipeline handoffs:
//  1. Fetching: hands off to downloader (dl.Wake()). Downloader work completes
//     and reports via completeFinalizedFile's reportDownloadComplete (`git grep -n 'func (app \*Application) completeFinalizedFile' internal/app/`) on download completion,
//     and yields via Stall (`git grep -n 'func (app \*Application) Stall(' internal/app/`) on stall, appWorkers.Abort
//     (`git grep -n 'func (w \*appWorkers) Abort' internal/app/`) on cancellation, or stopWorkers (`git grep -n 'func (app \*Application) stopWorkers' internal/app/`)
//     on shutdown. A job already complete at launch, and not admitted to
//     post-processing, is instead reported by runFetch itself, through
//     reportDownloadComplete. A job admitted from Fetching gets neither
//     report: its post-processing run releases the claim, on the paths
//     item 3 lists.
//  2. Assessing: discharges directly within runAssess via AdvanceFrom (intact,
//     repairable, or deferred recovery) or FinishedJob(OutcomeFailed) (hopeless).
//  3. Repairing/Extracting/Finalizing: hands off to postProcessor.Process
//     (enqueuePostProc). Post-processing completes and yields via
//     jobFinalizer.persistAndCommit (`git grep -n 'func (f \*jobFinalizer) persistAndCommit' internal/app/`),
//     via jobFinalizer.cancelled (`git grep -n 'func (f \*jobFinalizer) cancelled' internal/app/`)
//     for a job postProcessor.CancelJob took or the hand-over refused as removed, or via Shutdown (`git grep -n 'func (app \*Application) Shutdown' internal/app/`).
//  4. Guard branches (missing app, missing job, app.stopping, unhandled states):
//     discharges synchronously via immediate Yielded.
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
	if r.app != nil && r.app.stopping.Load() {
		if r.report != nil {
			_ = r.report.Yielded(id)
		}
		return
	}
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

// runFetch hands off work to the downstream downloader pool by poking
// dl.Wake(). Downloader worker goroutines are cancelled and drained by
// downloader.Stop during Application.Shutdown.
//
// A job that is already complete when its Fetching worker launches is
// reported here instead, through reportDownloadComplete, the owner
// completeFinalizedFile reports through. That report follows a file
// completion, and the downloader sends a complete job no article
// (ForEachUnfinishedArticle skips every Complete file), so without this such a
// job would hold its lease at Fetching with no report to move it on. Reporting
// it sends it to post-processing through Assessing, whose runAssess gives the
// on-demand par2 verdict. If both reports are made, the second returns
// ErrStaleReport and changes nothing. reportDownloadComplete makes no report
// for a job already admitted to post-processing.
func (r *appRunner) runFetch(_ context.Context, id string) {
	if r.app == nil {
		if r.report != nil {
			_ = r.report.Yielded(id)
		}
		return
	}
	var j *job.Job
	if r.app.dispatcher != nil {
		j, _ = r.app.dispatcher.Job(id)
	}
	if j == nil {
		if r.report != nil {
			_ = r.report.Yielded(id)
		}
		return
	}
	if r.report != nil {
		if reported, err := r.app.reportDownloadComplete(j, r.report); reported {
			r.logAdvance(j, job.Fetching, job.Assessing, err)
			return
		}
	}

	r.app.mu.Lock()
	dl := r.app.downloader
	r.app.mu.Unlock()
	if dl != nil {
		if w, ok := dl.(interface{ Wake() }); ok {
			w.Wake()
		}
	}
}

// runAssess executes assessment work synchronously in-goroutine and passes ctx
// to maybeReleaseRecoveryVolumes(ctx, j). It directly reports completion via
// FinishedJob or its verdict via AdvanceFrom. It resolves the job once, and
// every call after that carries the instance it resolved.
func (r *appRunner) runAssess(ctx context.Context, id string) {
	if r.app == nil {
		if r.report != nil {
			_ = r.report.Yielded(id)
		}
		return
	}
	var j *job.Job
	if r.app.dispatcher != nil {
		j, _ = r.app.dispatcher.Job(id)
	}
	if j == nil {
		if r.report != nil {
			_ = r.report.Yielded(id)
		}
		return
	}

	if r.app.maybeReleaseRecoveryVolumes(ctx, j) {
		r.advance(j, job.Fetching)
		return
	}

	repairState := j.RepairState()
	if repairState.Hopeless() {
		r.failHopeless(j)
		return
	}

	if repairState == job.RepairPossible || repairState == job.RepairUnknown {
		r.advance(j, job.Repairing)
		return
	}

	r.advance(j, job.Extracting)
}

// failHopeless is runAssess's verdict for a job par2 cannot repair: it hands j
// to post-processing with its failure reason and settles it OutcomeFailed.
// Both calls carry j, so a verdict on an instance that has left the dispatcher
// reaches no later instance registered under its ID.
func (r *appRunner) failHopeless(j *job.Job) {
	r.app.maybeFinalizeJob(j, failMsgForJob(j))
	if r.report != nil {
		_ = r.report.FinishedJob(j, job.OutcomeFailed)
	}
}

// advance reports runAssess's verdict: the work of Assessing is done and the
// job continues to next. The report records next and releases the job in one
// dispatcher call, so no tick can move the job between the two.
func (r *appRunner) advance(j *job.Job, next job.State) {
	if r.report == nil {
		return
	}
	r.logAdvance(j, job.Assessing, next, r.report.AdvanceFrom(j, job.Assessing, next))
}

// logAdvance logs an AdvanceFrom report's refusal, by cause.
func (r *appRunner) logAdvance(j *job.Job, from, next job.State, err error) {
	switch {
	case err == nil:
	case errors.Is(err, dispatch.ErrStaleReport) || errors.Is(err, dispatch.ErrNotFound):
		// The job left from, was cancelled, or was removed; whatever moved
		// it released it.
		r.log.Debug("runner: report not recorded", "job", j.ID(), "from", from, "next", next, "error", err)
	default:
		r.log.Warn("runner: report not recorded", "job", j.ID(), "from", from, "next", next, "error", err)
	}
}

// runPostProc hands off work to the downstream postProcessor subsystem pool via
// enqueuePostProc. Post-processor worker goroutines are cancelled and drained by
// postProcessor.Stop during Application.Shutdown.
func (r *appRunner) runPostProc(_ context.Context, id string, _ job.State) {
	if r.app == nil {
		if r.report != nil {
			_ = r.report.Yielded(id)
		}
		return
	}
	var j *job.Job
	var hdr dispatch.Header
	if r.app.dispatcher != nil {
		if row, ok := r.app.dispatcher.Row(id); ok {
			hdr = row.Header
		}
		j, _ = r.app.dispatcher.Job(id)
	}
	if j == nil || r.app.postProcessor == nil {
		if r.report != nil {
			_ = r.report.Yielded(id)
		}
		return
	}

	// No yield when enqueuePostProc refuses the job as already admitted. The
	// admitted run releases the launch claim on the paths item 3 of
	// appRunner's doc lists. Before the admitted run hands the job over, Has
	// is false: a dispatcher cancel at Repairing reaches appWorkers.Abort,
	// which then releases the claim, and one at Extracting or Finalizing does
	// not interrupt the run, which hands the job over as usual. A job a
	// RemoveJob took is refused at the hand-over, and jobFinalizer.cancelled
	// releases its claim. Nor when the instance's admission has ended:
	// finalize and cancelled each latch the cancel (CancelJob) and yield
	// before they end it, so a launch that took its claim before the cancel
	// had it cleared by that yield, and no later launch starts.
	r.app.enqueuePostProc(j, hdr, failMsgForJob(j))
}
