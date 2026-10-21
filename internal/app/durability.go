package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// handleArticlesUnwritten returns every article a failed write rolled back to
// Outstanding.
//
// The write did not happen, so the articles must be fetched again — but their
// Emitted bits are still set from dispatch, and ForEachUnfinishedArticle skips
// a set Emitted bit. Nothing else clears them on this path: the recorder does
// not see them (it records articles whose write succeeded), no
// Job.MarkArticleFailed names them, and eviction keeps job.progress, so
// pause and resume do not clear them either. Left alone they are stranded for
// the life of the process, at any residency, until something clears them. A
// restart clears them because the bit is never persisted: JobProgress is not
// serialized, and the restarted process builds each job's progress afresh
// with no Emitted bit set (internal/job/progress.go) — and a downloader reload
// clears them in-process once the assembler has quiesced.
//
// It takes a set, though each call the assembler makes today names one article
// (`git grep -n 'a\.noteArticlesUnwritten(' -- 'internal/assembler/*.go'
// ':!*_test.go'` returns 2 lines, each passing `[]int32{req.ArtIdx}`).
//
// R17: returns each article to Outstanding by clearing its emitted bit. The
// articles are not marked failed (A1) and their bytes are not charged against
// par2; the next dispatch cycle will pick them up again.
//
// It clears Emitted only, and a failed fsync does not reach it: the articles
// that fsync covered were marked Done when their WriteAt returned, and their
// Done bits and buffered rows go with the untrust of their file
// (handleFileUntrusted).
//
// Thread-safe: clears the emitted bits under the job lock. It does NOT touch
// the assembler.
func (app *Application) handleArticlesUnwritten(jobID string, fileIdx int, artIdxs []int32) {
	j, ok := app.dispatcher.Job(jobID)
	if !ok {
		app.log.Debug("unwritten articles not returned to Outstanding; the job has left the queue",
			"job", jobID, "fileidx", fileIdx, "articles", len(artIdxs))
		return
	}
	for _, artIdx := range artIdxs {
		if err := j.ClearArticleEmitted(int(artIdx)); err != nil {
			app.log.Warn("return unwritten article to Outstanding",
				"job", jobID, "fileidx", fileIdx, "artidx", artIdx, "err", err)
		}
	}
}

// handleWriteFault is the assembler's Options.OnWriteFault, and it routes the
// fault (R18/A1): a permanent condition fails the job, anything else stalls
// it.
//
// No article is named or marked failed. Returning the rolled-back articles to
// Outstanding is handleArticlesUnwritten's job, which the assembler calls with
// the whole set, and untrusting a file whose finish failed is
// handleFileUntrusted's.
//
// # Why the routing does not happen here
//
// This runs on the ASSEMBLER'S WORKER GOROUTINE, and both branches block on
// that same goroutine if run inline:
//
//   - Permanent → Application.Fail → finalizeRegistered → CloseJobHandles, which
//     enqueues a control message on a.reqs and waits for the worker that is
//     calling it. A guaranteed self-deadlock, resolved only by the 5s
//     closeHandlesTimeout expiring — after which the handles are still open,
//     so the NFS silly-rename that call exists to prevent is not prevented
//     either. enqueuePostProc then also runs a synchronous recorder flush
//     to SQLite and enqueues post-processing, both on the worker.
//   - Retryable → Application.Stall → Dispatcher.PauseInstance, which updates intent
//     manifest from disk and calls into SQLite. On the worker, that is the
//     single goroutine every write for every job passes through.
//
// The assembler's own OnFileComplete comment documents this hazard, and
// app.go's onFileComplete guards against it by handing the work to another
// goroutine. This callback did not.
//
// wg.Add during Wait is ORDINARILY safe because this runs on the assembler
// worker, which Shutdown stops (stopWorkers) before joinAndStop's app.wg.Wait().
//
// "Ordinarily" is doing real work in that sentence, and an earlier version
// omitted it. waitBounded ABANDONS its step when the budget expires: it logs
// "shutdown step exceeded budget; abandoning" and returns while the goroutine
// it was waiting on runs on. So a wedged mount that keeps Assembler.Stop past
// its 15s budget lets Shutdown proceed to app.wg.Wait() with the worker still
// draining — and a write fault raised after that point reaches this function
// and Adds to a WaitGroup that already has a waiter, which panics and takes
// the process down before Shutdown's final recorder flush.
//
// The window is narrow and this is not the place to close it; what matters
// here is that the claim is bounded rather than absolute, so nobody builds on
// it. Assembler.syncAndCloseAll declines to route faults at all for exactly
// this reason.
//
// # The instance is resolved here, not in the goroutine
//
// Stall and Fail act on the instance they are given, through the dispatcher's
// instance doors. It is looked up on the worker, while the write that faulted
// is being processed, so a retry registered under the same ID before the
// goroutine runs is left alone (#914). The lookup identifies the attempt as
// well as the worker's own queue does: a write of an earlier attempt still
// blocked in Assembler.WriteArticle when a retry's ForgetJob reached the
// worker would fault against the retry.
func (app *Application) handleWriteFault(jobID string, _ int, f *storagefault.Fault) {
	j, ok := app.dispatcher.Job(jobID)
	if !ok {
		app.log.Warn("storage fault for a job that has left the queue; nothing to stall or fail",
			"job", jobID, "fault", f.Error())
		return
	}
	app.wg.Go(func() {
		if app.writeFaultRoutedHook != nil {
			app.writeFaultRoutedHook(j)
		}
		if f.Permanent {
			app.Fail(j, f)
			return
		}
		app.Stall(j, f)
	})
}

// handleArticleRejected records an article the assembler refused as
// permanently failed.
//
// Permanently, and not returned to Outstanding, because the reason is a
// property of what the server sent: the offset comes from the article's own
// yEnc header, so a re-fetch of the same article yields the same rejection.
// Job.MarkArticleFailed charges its bytes against the job's par2 recovery
// budget and clears the Emitted bit as part of resolving the article —
// without it the job waits forever on something nothing will re-dispatch. A
// job evicted after Stall paused it still records the failure, but the byte
// charge waits for the next hydration: see Job.MarkArticleFailed.
//
// uncovered is the assembler's count of the article's bytes no written article
// covers, or assembler.WholeArticle, which maps to job.WholeArticle. A count
// is the charge Job.MarkArticleFailed records, so only those bytes go to par2:
// the rest of the range is on disk, written by another article (#432). A
// count of zero leaves the article out of the early-abort rate (#844).
//
// This is the other side of the A1 split from handleWriteFault: that one
// stalls the job and touches no article, this one fails the article and
// touches no job state.
func (app *Application) handleArticleRejected(jobID string, fileIdx int, artIdx int32, reason string, uncovered int64) {
	app.log.Warn("article rejected by the assembler; recording it as permanently failed",
		"job", jobID, "fileidx", fileIdx, "artidx", artIdx, "reason", reason, "uncovered", uncovered)
	j, ok := app.dispatcher.Job(jobID)
	if !ok {
		app.log.Debug("rejected article not recorded; the job has left the queue",
			"job", jobID, "artidx", artIdx)
		return
	}
	charge := uncovered
	if uncovered == assembler.WholeArticle {
		charge = job.WholeArticle
	}
	if err := j.MarkArticleFailed(int(artIdx), charge); err != nil {
		app.log.Warn("record rejected article as permanently failed",
			"job", jobID, "fileidx", fileIdx, "artidx", artIdx, "err", err)
	}
}

// Stall parks a job on a retryable storage fault and surfaces why (R19, R27).
//
// No article is marked failed, and that is the whole of A1. A full disk is a
// condition of the device, not evidence about any article: attributing it to
// the remote article would burn its retry budget, inflate the job's
// failed-byte count and degrade its reported health (R21) — all from something
// the user often fixes in ten seconds. The articles stay Outstanding and are
// re-fetched when the job resumes.
//
// The job is paused rather than left running because a running job would keep
// dispatching articles into a device that cannot take them, turning one
// surfaced fault into a flood of them.
//
// # Nothing is parked while the process is stopping
//
// A pause taken during shutdown is the one that cannot be undone. Shutdown
// persists it, the stall list that would re-evaluate it is in-memory and dies
// with the process, and nothing at startup resumes it. The job comes back
// Paused forever.
//
// The test is whether the PROCESS is stopping, not what the error was. A
// wedged mount produces deadline faults of its own while running, and those
// must still park the job with a reason — otherwise it sits at 99% with
// nothing surfaced, which A2 forbids.
//
// Both an explicit flag and the context are consulted. Shutdown sets the flag
// before its first step, while it cancels app.ctx in joinAndStop, after the
// assembler has stopped, so the flag is what covers a fault raised while the
// assembler drains and closes its files. The context test covers a SIGTERM-cancelled parent context, which
// arrives without Shutdown having been entered.
//
// # Only the instance it is given
//
// j is the attempt the fault belongs to. A j no longer registered under its ID
// parks nothing: the stall record is keyed by ID, and a later attempt
// registered under it is not the one that faulted. For a j handed to
// post-processing, see handOverStall.
func (app *Application) Stall(j *job.Job, f *storagefault.Fault) {
	app.stall(j, f, stallRetryable)
}

// parkVerifyFault is the residency's parked callback: Stall for a
// verification that could not complete, recorded as stallVerify so that only
// a resume of the job lifts it (docs/durability-contract.md §3).
func (app *Application) parkVerifyFault(j *job.Job, f *storagefault.Fault) {
	app.stall(j, f, stallVerify)
}

// stall is the body of Stall and parkVerifyFault; kind is what may lift it.
func (app *Application) stall(j *job.Job, f *storagefault.Fault, kind stallKind) {
	jobID := j.ID()
	if app.stopping.Load() || (app.ctx != nil && app.ctx.Err() != nil) {
		// Not silent (A2): the condition is real, and the next run's
		// hydration verifies the job's files regardless.
		app.log.Warn("storage fault during shutdown; the job is not parked for it",
			"job", jobID, "fault", f.Error())
		return
	}
	if !app.registered(j) {
		app.log.Info("storage fault for an attempt that has left the queue; nothing is parked",
			"job", jobID, "fault", f.Error())
		return
	}
	// Recorded BEFORE the pause. R19 requires the condition to be
	// re-evaluated at all, which needs a list of what is parked — the
	// dispatcher row carries no separate copy of this reason (StallReason,
	// read via app.StallReason, is the queue listing's source for it; see
	// reevaluateStall).
	//
	// The pause is claimed only when the user has not already paused the job:
	// the intent is read before the pause. IntentPause there is not
	// necessarily the user's: AddJob pauses a duplicate NZB and a
	// paused-priority ingest, and a re-stall meets Stall's own earlier pause,
	// which stays ours because setStallReasonLocked never clears parked.
	// The read holds the admission lock but no dispatcher lock, so two races
	// with the user are accepted: a user pause landing between the read and
	// the pause is claimed by us and resumed once the fault clears, and a
	// user resume in the same window leaves the pause on a job nobody owns
	// (docs/durability-contract.md).
	if app.noteStallUnlessHandedOff(j, f, kind) {
		app.log.Warn("job stalled by a storage fault", "job", jobID, "fault", f.Error())
	} else {
		// Handed off (see handOverStall): no record, since a record makes
		// reevaluateStall resume the ID, but the pause below is still made. A
		// verify fault from the residency's parked callback relies on it to
		// stop Dispatcher.reconcileResidency re-hydrating the job every tick.
		app.offerFaultToAdmission(j, f, kind.source())
	}
	// The pause is latched whatever the state, and PauseInstance releases only
	// a Fetching job's lease, the worker whose writes the fault
	// interrupts. A job that has moved on — Assessing, say — keeps its
	// worker and its resources.
	// That worker finishes and reports through AdvanceFrom, and the pause
	// then gates the move.
	//
	// A cancel latched first refuses the pause with job.ErrIntentLatched:
	// a fault from writes already queued can land between RemoveJob's
	// Cancel and its Remove. The job is going away, so that is no failure
	// of the stall.
	if err := app.dispatcher.PauseInstance(j); !errors.Is(err, job.ErrIntentLatched) {
		warnUnlessGone(app.log, "stall: pausing the job failed", jobID, err)
	}
	app.emit(Event{Type: "queue_updated", NzoID: jobID})
}

// faultReason is the failure reason a storage fault gives the job it fails:
// Fail's permanent fault (R20), and every fault offerFaultToAdmission offers.
func faultReason(f *storagefault.Fault) string {
	return "Failed: " + f.Error()
}

// Fail stops a job on a permanent storage fault (R20).
//
// Still no article is marked failed. A read-only filesystem says nothing about
// any article's availability, and recording it as article damage would make
// the job's health figure describe the disk instead of the download.
//
// The hand-over to post-processing (finalizeRegistered) is how every other
// terminal condition leaves the queue — the job carries its reason into
// history rather than sitting in the queue in a state nothing will move it out
// of. A job at Assessing, or due there, is not handed over here: the hand-over
// leaves the reason for the job's Assessing
// worker, which hands the job over itself
// (postProcAdmissions.admitUnlessAssessing), so the finalize does not run
// beside runAssess. The hand-over resumes such a job if Stall paused it, so
// that worker launches; a job the user paused waits for the user's resume
// (handOverStall).
//
// # No stopping guard, unlike Stall
//
// Stall declines while stopping because its pause is persisted and nothing that
// could undo it outlives the process. Fail advances no position: neither it nor
// enqueuePostProc calls SetNext, Transition, Cross or Finish. On this route the
// job is settled by the finalizer once the post-processor has run it, or, for
// a reason left for the Assessing worker, by that worker's FinishedJob, as for
// a hopeless verdict. The hand-off, or the reason left for the worker, is held
// in memory. So a Fail during shutdown either files the job through a
// post-processor that is still running, or the hand-off, or the reason left
// for the worker, dies with the process and the job restarts at the state it
// was in, its outstanding articles offered again. Of the fields Fail writes,
// Header.FailReason is the persisted one, and a restarted job keeps it until
// it leaves the queue.
//
// j is the attempt the fault belongs to. A j no longer registered under its ID
// fails nothing. Otherwise the reason and the hand-over go through instance
// doors, and the stall record, which is keyed by ID, is cleared by the
// hand-over only once finalizeRegistered has found j holding the ID.
func (app *Application) Fail(j *job.Job, f *storagefault.Fault) {
	jobID := j.ID()
	reason := faultReason(f)
	if !app.registered(j) {
		app.log.Info("permanent storage fault for an attempt that has left the queue; nothing is failed",
			"job", jobID, "fault", f.Error())
		return
	}
	app.log.Error("job failed by a permanent storage fault", "job", jobID, "fault", f.Error())
	// enqueuePostProc hands the job to the postproc queue rather than
	// removing it synchronously, so history.Entry.FailMessage (this
	// reason's eventual permanent home) does not exist yet — this is
	// the only live signal for the window until it does.
	warnUnlessGone(app.log, "record the fail reason", jobID, app.dispatcher.SetFailReason(j, reason))
	// A permanent fault is not re-evaluated (R20): the hand-over takes the
	// job off the stalled list, and resumes it if Stall paused it
	// (handOverStall).
	app.finalizeRegistered(j, reason, true)
}

// runStallRecheck is R19's cadence: every parked job is re-evaluated on an
// interval, and on user action (ReevaluateStalls).
func (app *Application) runStallRecheck(ctx context.Context) {
	recheck := app.stallRecheckInterval
	if recheck <= 0 {
		recheck = stallRecheckInterval
	}
	stallTicker := time.NewTicker(recheck)
	defer stallTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stallTicker.C:
			app.reevaluateStalls(ctx)
		case <-app.stallKick:
			app.reevaluateStalls(ctx)
		}
	}
}

// dropJobAlreadyInHistory removes a queue job that has already been filed in
// history.
//
// Reached at startup by a job that crashed between MoveToHistory and the queue
// removal that follows it. The queue row is a duplicate of an entry that is
// already the record. It runs before the dispatcher's first tick
// (reconcileBeforeFirstTick), so a duplicate it removes is never routed
// onward.
//
// Only history.ErrNotFound establishes that the job is not in history, and
// the tick then routes it like any other. A failed lookup establishes
// nothing, so the job is paused with an operational error rather than
// routed: a tick would otherwise post-process a job that may already be
// filed, and its finalize would try to file it again. The pause keeps its
// manifest and rows, and lasts until an operator resumes it.
//
// Its rows and manifest go through reclaim, which keeps a FAILED entry's
// job_files and written_articles for a retry the way every departure does.
func (app *Application) dropJobAlreadyInHistory(ctx context.Context, jobID string) {
	// With no history database there is no history to find the job in. The
	// check lives here rather than at the call site, so the method answers
	// correctly for any caller instead of relying on each one to remember.
	if app.historyRepo == nil || app.historyRepo.DB() == nil {
		return
	}
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
	entry, err := app.historyRepo.Get(dbCtx, jobID)
	dbCancel()
	if err != nil {
		// ErrNotFound and "the lookup failed" are different answers. A
		// timeout, a lock or an I/O error is the absence of knowledge, so
		// nothing is deleted on it, and nothing is routed on it either.
		if !errors.Is(err, history.ErrNotFound) {
			app.holdUnreconciledJob(jobID, err)
		}
		return
	}
	app.log.Info("found job already in history but still in queue, removing",
		"job", jobID, "status", entry.Status)
	// A failed Remove leaves the queue row in place, and the reclaim below
	// then keeps the manifest and every row: the dispatcher still holds the
	// job and the rule sees its row (#376). The next startup reconciles it.
	// A Remove that fails after its Cancel leaves the job cancelled and
	// registered (Dispatcher.Remove's retry contract), so the tick does not
	// route it onward.
	//
	// Bounded, because the caller's context is not. Start receives a
	// signal.NotifyContext with no deadline (cmd/gonzbd/main.go), and Remove
	// waits on worker launch, on live leases and on the store -- so an
	// unbounded call here blocks startup until an operator sends a signal.
	// Thirty seconds matches the bound RemoveJob puts on this same call.
	rmCtx, rmCancel := context.WithTimeout(ctx, 30*time.Second)
	rmErr := app.dispatcher.Remove(rmCtx, jobID)
	rmCancel()
	if rmErr != nil {
		app.log.Error("failed to remove duplicate job from dispatcher; leaving its "+
			"manifest and durability rows for the next startup to reconcile",
			"job", jobID, "err", rmErr)
	}
	// Detached, for the reason RemoveJob's own cleanup is: the queue row is
	// already gone, so a cancellation landing here strands the rows with
	// nothing to reclaim them until the next start. ctx is the startup
	// context, which ends on SIGINT and on the startup deadline.
	//
	// The two steps above are NOT detached, and the asymmetry is the point.
	// The history Get and dispatcher.Remove are allowed to fail because a
	// failure destroys nothing, and the next startup reconciles the job
	// again. Past a SUCCESSFUL dispatcher.Remove there is no next time, which
	// is why this one is detached instead.
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	app.reclaim(delCtx, jobID)
	delCancel()
}

// holdUnreconciledJob pauses a job whose history lookup failed, so that no
// tick routes it onward, and records why on the job for the operator.
//
// For a registered job PauseJob returns ErrIntentLatched, which leaves the job
// unpaused — a cancelled job is not routed onward either — or an error from
// returning its Fetching lease, which comes after the pause has latched.
func (app *Application) holdUnreconciledJob(jobID string, lookupErr error) {
	app.log.Error("history lookup failed; pausing this job, which may already be filed, "+
		"until an operator resumes it", "job", jobID, "err", lookupErr)
	if err := app.dispatcher.PauseJob(jobID); err != nil {
		app.log.Error("failed to pause a job whose history lookup failed",
			"job", jobID, "err", err)
		return
	}
	warnUnlessGone(app.log, "record the operational error", jobID, app.dispatcher.SetOperationalError(jobID,
		"history lookup failed at startup: this job may already be in history; resume it only once it is known not to be"))
}

// reclaim applies durability's reclaim rule to the named jobs, then unlinks
// the manifest of each one that has neither a registered dispatcher entry nor
// a persisted dispatch_jobs row. Call it after the
// state change a departure depends on: the rule reads the queue and history as
// they are, so it cannot reclaim a job something still reaches, and calling it
// after a departure that failed or was made by someone else is harmless.
//
// Logged, not returned. Every caller has either already departed, with no one
// left to tell, or is already returning the error that matters. A failure
// leaves rows and manifests for the startup sweep (sweepOrphans).
//
// ctx is the caller's, and should be detached and bounded: past a departure,
// nothing retries this call before the next startup.
func (app *Application) reclaim(ctx context.Context, id string, more ...string) {
	if app.durable != nil {
		if err := app.durable.Reclaim(ctx, id, more...); err != nil {
			app.log.Warn("could not reclaim a departed job's rows; the next startup's sweep will",
				"job", id, "more", len(more), "err", err)
		}
	}
	app.unlinkDepartedManifests(append([]string{id}, more...))
}

// unlinkDepartedManifests unlinks the manifest of each named job that has
// neither a registered dispatcher entry nor a persisted dispatch_jobs row
// (including rows skipped during Store.Load or Dispatcher.restore), which is
// the disk half of the rule: a manifest's lifetime is exactly its queue row's.
func (app *Application) unlinkDepartedManifests(ids []string) {
	dir := manifestDir(app.config.GetGeneral().AdminDir)
	for _, jobID := range ids {
		if _, held := app.dispatcher.Job(jobID); held {
			continue
		}
		if app.hasUnrestoredQueueRow(jobID) {
			continue
		}
		if err := removeManifestIn(dir, jobID); err != nil && !os.IsNotExist(err) {
			app.log.Warn("could not unlink a departed job's manifest", "job", jobID, "err", err)
		}
	}
}

// hasUnrestoredQueueRow reports whether dispatch_jobs holds a row for a job ID
// that is not registered in the dispatcher (a row skipped by Store.Load or
// Dispatcher.restore). It delegates the query to dispatchstore.Store.Has with a
// bounded context and fails closed (returns true) on a read error so a
// transient database fault during the startup orphan sweep never unlinks a
// skipped job's manifest.
func (app *Application) hasUnrestoredQueueRow(jobID string) bool {
	if _, held := app.dispatcher.Job(jobID); held {
		return false
	}
	if app.durable == nil || app.historyRepo == nil || app.historyRepo.DB() == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exists, err := dispatchstore.New(app.historyRepo.DB(), app.log).Has(ctx, jobID)
	if err != nil {
		app.log.Warn("could not check dispatch_jobs before unlinking manifest; keeping manifest",
			"job", jobID, "err", err)
		return true
	}
	return exists
}

// sweepOrphans reclaims what every missed or crash-interrupted departure left:
// the rows of every unreachable job, and the manifest of every job that has
// neither a registered dispatcher entry nor a persisted dispatch_jobs row.
//
// Startup only, after the dispatcher has restored the queue and before
// anything can call Admit (durability.Store.SweepOrphans says why).
func (app *Application) sweepOrphans(ctx context.Context) {
	if app.durable != nil {
		if err := app.durable.SweepOrphans(ctx); err != nil {
			app.log.Warn("startup sweep of unreachable durability rows failed", "err", err)
		}
	}
	entries, err := os.ReadDir(manifestDir(app.config.GetGeneral().AdminDir))
	if err != nil {
		if !os.IsNotExist(err) {
			app.log.Warn("startup sweep could not list the manifests", "err", err)
		}
		return
	}
	var ids []string
	for _, e := range entries {
		if jobID, ok := strings.CutSuffix(e.Name(), manifestSuffix); ok && !e.IsDir() {
			ids = append(ids, jobID)
		}
	}
	// The manifests only. SweepOrphans above already applied the rule to every
	// job in the database, so reclaiming these ids again would delete nothing.
	app.unlinkDepartedManifests(ids)
}

// durabilityStore is what Application calls on durability.Store: the
// job_files seed, reads of the article record, and the reclaim rule the
// departure paths are built from. It is declared here, at its consumer, so a
// test can substitute a store that fails or records.
//
// It has no method that updates the record: the recorder is its writer, through
// recordStore.
type durabilityStore interface {
	Admit(ctx context.Context, jobID string, files []durability.AdmitFile) error
	FileRows(ctx context.Context, jobID string) ([]durability.FileRow, error)
	WrittenRows(ctx context.Context, jobID string) ([]durability.WrittenRow, error)
	Reclaim(ctx context.Context, id string, more ...string) error
	SweepOrphans(ctx context.Context) error
}

var _ durabilityStore = (*durability.Store)(nil)
