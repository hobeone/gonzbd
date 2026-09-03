package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// This file used to open with manifestArticleMap, an adapter that answered
// the two manifest questions the assembler cannot: how many articles a file
// holds, and where a global article index sits within it. Both existed
// exclusively to place bits in a per-file durable bitmap.
//
// A durable run carries FirstArtIdx and LastArtIdx directly, so there is no
// bitmap to size and no ordinal to convert, and the whole vertical went with
// them — the adapter, assembler.ArticleMap, and the two SyncTarget methods
// they backed. syncTargetFor below is what is left of it.

// AckDurable satisfies durability.Acker: it acknowledges durable articles on
// the job's progress record.
func (app *Application) AckDurable(p durability.DurableProof) error {
	if app.dispatcher != nil {
		if j, ok := app.dispatcher.Job(p.JobID()); ok {
			if _, _, err := j.AckDurable(p); err != nil {
				return err
			}
			if app.checkpointer != nil {
				app.checkpointer.Mark(j)
			}
		}
	}
	return nil
}

// handleArticlesUnwritten returns every article a failed write rolled back to
// Outstanding.
//
// The write did not happen, so the articles must be fetched again — but their
// Emitted bits are still set from dispatch, and ForEachUnfinishedArticle skips
// a set Emitted bit. Nothing else clears them on this path: no Drain reports
// them, no Job.MarkArticleFailed names them, and eviction keeps job.progress, so
// pause and resume do not clear them either. Left alone they are stranded for
// the life of the process, at any residency, and only a restart's
// ClearAllEmitted recovers them.
//
// It takes a SET rather than one article, and that is the point. The assembler
// used to carry a single index alongside the fault, so a batch failure — a
// coalesced run, a drain, a cache displacement — reported only whichever
// article happened to be first in the batch.
//
// R17: returns each article to Outstanding by clearing its emitted bit. The
// articles are not marked failed (A1) and their bytes are not charged against
// par2; the next dispatch cycle will pick them up again.
//
// Thread-safe: clears the emitted bits under the job lock. It does NOT touch
// the assembler.
func (app *Application) handleArticlesUnwritten(jobID string, _ int, artIdxs []int32) {
	for _, artIdx := range artIdxs {
		if app.dispatcher != nil {
			if j, ok := app.dispatcher.Job(jobID); ok {
				_ = j.ClearArticleEmitted(int(artIdx))
			}
		}
	}
}

// handleWriteFault is the assembler's Options.OnWriteFault, and it routes the
// fault on the same rule the barrier uses (R18/A1): a permanent condition
// fails the job, anything else stalls it.
//
// No article is named or marked failed. Returning the rolled-back articles to
// Outstanding is handleArticlesUnwritten's job, which the assembler calls with
// the whole set — including on the paths where the BARRIER routes the fault
// and this function never runs at all.
//
// # Why the routing does not happen here
//
// This runs on the ASSEMBLER'S WORKER GOROUTINE, and both branches block on
// that same goroutine if run inline:
//
//   - Permanent → Application.Fail → maybeFinalize → CloseJobHandles, which
//     enqueues a control message on a.reqs and waits for the worker that is
//     calling it. A guaranteed self-deadlock, resolved only by the 5s
//     closeHandlesTimeout expiring — after which the handles are still open,
//     so the NFS silly-rename that call exists to prevent is not prevented
//     either. maybeFinalize then also does a queue.Save and enqueues
//     post-processing, both on the worker.
//   - Retryable → Application.Stall → Dispatcher.PauseJob, which updates intent
//     manifest from disk and calls into SQLite. On the worker, that is the
//     single goroutine every write for every job passes through.
//
// The assembler's own OnFileComplete comment documents this hazard, and
// app.go's onFileComplete guards against it by handing the work to another
// goroutine. This callback did not.
//
// wg.Add during Wait is ORDINARILY safe because this runs on the assembler
// worker, which Shutdown joins at step 3 — before app.wg.Wait() at step 4.
//
// "Ordinarily" is doing real work in that sentence, and an earlier version
// omitted it. waitBounded ABANDONS its step when the budget expires: it logs
// "shutdown step exceeded budget; abandoning" and returns while the goroutine
// it was waiting on runs on. So a wedged mount that keeps Assembler.Stop past
// its 15s budget lets Shutdown proceed to app.wg.Wait() with the worker still
// draining — and a write fault raised after that point reaches this function
// and Adds to a WaitGroup that already has a waiter, which panics and takes
// the process down before Shutdown's final queue.Save.
//
// The window is narrow and this is not the place to close it; what matters
// here is that the claim is bounded rather than absolute, so nobody builds on
// it. Assembler.drainAndCloseAll declines to route faults at all for exactly
// this reason.
func (app *Application) handleWriteFault(jobID string, _ int, f *storagefault.Fault) {
	app.wg.Go(func() {
		if f.Permanent {
			app.Fail(jobID, f)
			return
		}
		app.Stall(jobID, f)
	})
}

// handlePostAnomaly surfaces a structural fault in what the servers served, so
// the user can tell a bad post from a bad disk or a bad connection (#379).
//
// It writes job.Warning, which QueueRow renders next to the job. That field is
// single-valued and has other writers — the stall reason (which QueueRow shows
// in PREFERENCE to it), the two durability warnings, and the claim-failure note
// — so this can be overwritten by a later condition, and Resume/Retry clear it.
// That is acceptable for a diagnostic: the log line in routeFaulted is the
// durable record, and the warning is the thing that makes a user look.
//
// A failure to record it is logged and dropped. A job that has left the queue
// has nothing to warn about, which is ordinary rather than a defect (A2).
//
// There are TWO sources, which the log line names rather than assumes. The
// assembler detects an exact-offset collision at accept time, before either
// article is written. The barrier detects it at COMPLETION, by comparing the
// bytes its recorded runs account for against the file's real size — see
// durability.PostAnomaly.
//
// They are not redundant, and the reason is not that their cases are disjoint.
// Within one process they are: the assembler resolves its collision's loser
// permanently failed, so nothing writes its bytes and no run records it.
// Across a RESTART the assembler's acceptedAt is empty, so two articles at the
// same offset can both be written and both recorded, and the barrier's sum
// then exceeds the file. It is still not a double report, because the
// assembler is blind in exactly that window — the window, not the offsets, is
// what keeps them from overlapping. What the barrier alone can see, in any
// window, is a RANGE overlap that shares no start offset.
func (app *Application) handlePostAnomaly(jobID string, fileIdx int, reason string) {
	app.postAnomaly(jobID, fileIdx, "assembler", reason)
}

// reportPostAnomalies routes the barrier's findings.
//
// Called on both barrier paths and always AFTER their per-job mutex is
// released — see the call sites. Nil and empty are the overwhelmingly common
// case and cost one branch.
func (app *Application) reportPostAnomalies(jobID string, found []durability.PostAnomaly) {
	for _, pa := range found {
		app.postAnomaly(jobID, int(pa.FileIdx), "barrier", pa.Reason)
	}
}

func (app *Application) postAnomaly(jobID string, fileIdx int, source, reason string) {
	app.log.Warn("post anomaly reported",
		"job", jobID, "fileidx", fileIdx, "source", source, "reason", reason)
	if app.dispatcher != nil {
		_ = app.dispatcher.SetWarning(jobID, reason)
	}
}

// handleArticleRejected records an article the assembler refused as
// permanently failed.
//
// Permanently, and not returned to Outstanding, because the reason is a
// property of what the server sent: the offset comes from the article's own
// yEnc header, so a re-fetch of the same article yields the same rejection.
// Ack is what charges its bytes against the job's par2 recovery budget and
// releases on-demand recovery volumes, and Job.MarkArticleFailed clears
// the Emitted bit as part of resolving the article — without it the job waits
// forever on something nothing will re-dispatch.
//
// This is the other side of the A1 split from handleWriteFault: that one
// stalls the job and touches no article, this one fails the article and
// touches no job state.
func (app *Application) handleArticleRejected(jobID string, fileIdx int, artIdx int32, reason string) {
	app.log.Warn("article rejected by the assembler; recording it as permanently failed",
		"job", jobID, "fileidx", fileIdx, "artidx", artIdx, "reason", reason)
	if app.dispatcher != nil {
		if j, ok := app.dispatcher.Job(jobID); ok {
			_ = j.MarkArticleFailed(int(artIdx))
			if app.checkpointer != nil {
				app.checkpointer.Mark(j)
			}
		}
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
// A pause taken during shutdown is the one that cannot be undone. Shutdown's
// final queue.Save PERSISTS it, the stall list that would re-evaluate it is
// in-memory and dies with the process, and the startup sweep skips the job
// because its phase is no longer active. The job comes back Paused forever.
//
// The trigger is ordinary rather than exotic: the clean-shutdown barrier runs
// under a fixed deadline, and on a queue with many open files the fsyncs
// exceed it, so every job it reaches raises a deadline-exceeded fault.
//
// The test is whether the PROCESS is stopping, not what the error was. A
// wedged mount produces a deadline of its own through barrierOpTimeout, and
// that one must still park the job with a reason — otherwise it sits at 99%
// with nothing surfaced, which A2 forbids. The two are distinguishable only by
// whether we are stopping, so that is what is asked.
//
// Both an explicit flag and the context are consulted, and the flag is the one
// that does the work. Shutdown runs the clean-shutdown checkpoint at step 2 and
// app.cancel() at step 4, so on the ordinary path ctx.Err() is still nil while
// the barrier that raises these faults is running — the guard was inert on the
// exact path it was written for. Only a SIGTERM-cancelled parent context
// reached it. The context test stays because that case is real and arrives
// without Shutdown having been entered.
func (app *Application) Stall(jobID string, f *storagefault.Fault) {
	if app.stopping.Load() || (app.ctx != nil && app.ctx.Err() != nil) {
		// Not silent (A2): the condition is real, and the next run's resume
		// sweep re-derives the job's state from disk regardless.
		app.log.Warn("storage fault during shutdown; the job is not parked for it",
			"job", jobID, "fault", f.Error())
		return
	}
	reason := "Stalled: " + f.Error()
	app.log.Warn("job stalled by a storage fault", "job", jobID, "fault", f.Error())
	// Recorded BEFORE the pause, and kept after it. The queue's own warning is
	// wiped by the Resume a re-evaluation performs, so it cannot be what the
	// re-evaluation reads to know the job is parked — and R19 requires the
	// condition to be re-evaluated at all, which needs a list of what is
	// parked. See reevaluateStall.
	app.noteStall(jobID, f)
	if app.dispatcher != nil {
		_ = app.dispatcher.PauseJob(jobID)
		_ = app.dispatcher.SetWarning(jobID, reason)
		_ = app.dispatcher.Yielded(jobID)
	}
	app.emit(Event{Type: "queue_updated", NzoID: jobID})
}

// Fail stops a job on a permanent storage fault (R20).
//
// Still no article is marked failed. A read-only filesystem says nothing about
// any article's availability, and recording it as article damage would make
// the job's health figure describe the disk instead of the download.
//
// maybeFinalize is how every other terminal condition leaves the queue — the
// job carries its reason into history rather than sitting in the queue in a
// state nothing will move it out of.
func (app *Application) Fail(jobID string, f *storagefault.Fault) {
	reason := "Failed: " + f.Error()
	app.log.Error("job failed by a permanent storage fault", "job", jobID, "fault", f.Error())
	// A permanent fault is not re-evaluated (R20): the job leaves the queue
	// with its reason, so keeping it on the stalled list would have the
	// re-evaluation resume a job that is on its way to history.
	app.clearStall(jobID)
	if app.dispatcher != nil {
		_ = app.dispatcher.SetWarning(jobID, reason)
	}
	app.maybeFinalize(jobID, reason)
}

// barrierLock is one job's barrier mutex, plus what is needed to drop it
// safely.
//
// The counters exist because the map entry's lifetime and the mutex's lifetime
// are not the same: a job can leave the assembler's business while a barrier
// for it is still running, and deleting the entry then was not merely untidy —
// it let a SECOND mutex be minted for the same job, which is the same as having
// no mutex at all.
type barrierLock struct {
	mu sync.Mutex
	// holders counts callers between jobBarrierLock and
	// releaseJobBarrierLock, guarded by Application.barrierMu.
	holders int
	// forgotten records a deletion deferred until the last holder is done.
	forgotten bool
}

// jobBarrierLock returns the mutex serialising barrier work for one job.
//
// Barrier.Run holds no lock of its own — it does I/O throughout, and this
// project bans I/O under a lock — so the cadence owner has to guarantee at
// most one barrier in flight per job. Drain is DESTRUCTIVE, so two concurrent
// barriers over one file split its articles between them: one gets what the
// writer was holding and the other gets none. Each then acks only its own
// half while both believe they checkpointed the file, and whichever confirms
// releases the reports the other never saw — so those articles are neither
// acked nor re-reported, and only a restart recovers them.
//
// The lock is per job, not global, because a barrier is a few dozen fsyncs
// and one job's slow mount must not park every other job's checkpoint.
//
// FinalizeFile takes it too. It is a barrier by another name — same drain,
// same RunStore.Commit — so it races Run for exactly the same reason.
func (app *Application) jobBarrierLock(jobID string) *sync.Mutex {
	app.barrierMu.Lock()
	defer app.barrierMu.Unlock()
	e, ok := app.jobBarrierMu[jobID]
	if !ok {
		e = &barrierLock{}
		app.jobBarrierMu[jobID] = e
	}
	// Counted while the caller holds it, so forgetJobBarrierState cannot
	// delete an entry a live barrier is using. See releaseJobBarrierLock.
	e.holders++
	return &e.mu
}

// releaseJobBarrierLock records that one holder is finished with a job's
// barrier mutex, and completes a deletion that was deferred while it held it.
//
// It exists because forgetJobBarrierState could delete a mutex a barrier was
// standing on. The next jobBarrierLock then minted a SECOND mutex for the same
// job, and two barriers ran concurrently over it — each draining half the
// file's articles and acking only its own half, with the loser's reports
// released by whichever cycle confirmed. Those articles are then neither
// acked nor re-reported, and only a restart recovers them.
//
// The delete is reachable from INSIDE a live barrier, which is what makes this
// more than hygiene: routeFault → stall.Fail → Application.Fail →
// maybeFinalize → enqueuePostProc → forgetJobBarrierState, all beneath the very
// checkpointJob call that holds the mutex.
//
// Called after Unlock rather than before, so the entry is not resurrected by a
// caller still inside its critical section.
func (app *Application) releaseJobBarrierLock(jobID string) {
	app.barrierMu.Lock()
	defer app.barrierMu.Unlock()
	e, ok := app.jobBarrierMu[jobID]
	if !ok {
		return
	}
	if e.holders > 0 {
		e.holders--
	}
	if e.holders == 0 && e.forgotten {
		delete(app.jobBarrierMu, jobID)
	}
}

// forgetJobBarrierState drops a departed job's lock, byte accumulator and
// last-barrier stamp.
//
// All three maps are keyed by job ID and would otherwise grow for the life of
// the process, one entry per job ever downloaded. Called from the same places
// pipeline.forgetJob is, which is where a job stops being the assembler's
// business.
func (app *Application) forgetJobBarrierState(jobID string) {
	app.barrierMu.Lock()
	defer app.barrierMu.Unlock()
	delete(app.jobBarrierBytes, jobID)
	delete(app.lastBarrier, jobID)
	// The mutex is NOT dropped while anyone holds it. Dropping it let the next
	// caller mint a second mutex for the same job, so two barriers could
	// split one job's destructive Drain between them — and the delete is
	// reachable from inside a live barrier, through
	// routeFault → Fail → maybeFinalize → enqueuePostProc. Marked instead, and
	// the last holder completes it. See releaseJobBarrierLock.
	//
	// The HAZARD CHANGED with the durable-runs design; this is not a
	// re-wording of the old one. The mutex used to guard a read-modify-write
	// of the file's extent, where the second commit overwrote the first and
	// the loser's acked articles were durable with no bit to say so. That is
	// closed structurally now: RunStore.Commit does its whole read-merge-write
	// inside ONE SQLite transaction, so two of them serialise rather than
	// interleave. What the mutex still guards is upstream of the store — Drain
	// hands each article to exactly one caller, so two concurrent barriers
	// split a file's articles and each acks only its half, while whichever
	// calls Confirm releases the reports the other never saw.
	e, ok := app.jobBarrierMu[jobID]
	if !ok {
		return
	}
	e.forgotten = true
	if e.holders == 0 {
		delete(app.jobBarrierMu, jobID)
	}
}

// noteJobBytes accumulates a job's freshly written bytes and asks for a
// barrier once the volume bound is crossed (B1's byte half).
//
// The two bounds answer different failure shapes and neither subsumes the
// other. The time bound is what limits rework on a slow link, where 30 seconds
// is a few articles; the byte bound is what limits it on a fast one, where 30
// seconds can be a gigabyte. The barrier fires on whichever arrives first.
//
// The counter resets when the barrier runs, not here, so a kick that cannot be
// delivered is not lost: the bytes stay counted and the next call tries again,
// or the interval tick collects them.
func (app *Application) noteJobBytes(jobID string, n int) {
	if n <= 0 || app.barrier == nil {
		return
	}
	app.barrierMu.Lock()
	app.jobBarrierBytes[jobID] += int64(n)
	crossed := app.jobBarrierBytes[jobID] >= app.checkpointBytes
	app.barrierMu.Unlock()
	// --- No lock held below this line ---
	if !crossed {
		return
	}
	select {
	case app.barrierKick <- jobID:
	default:
		// The checkpoint loop is busy. Dropping the kick costs nothing: the
		// accumulator was not reset, so the next article re-raises it, and
		// the interval tick covers the job regardless.
	}
}

// syncTargetFor builds the barrier's view of one job's open files, or nil when
// the job has no resident manifest.
//
// The manifest is no longer READ here — the target it builds needs nothing
// from it — but the residency check stays, because it is the thing that
// decides whether a checkpoint should run at all. A job whose manifest has
// been evicted is not downloading, so it has nothing open to checkpoint; the
// barrier's own ack would fail on a non-resident job anyway (Queue.AckDurable
// requires residency), and reaching it only to fail there would turn an
// ordinary event into a logged error.
//
//nolint:ireturn // the barrier takes this interface; there is no concrete type to return
func (app *Application) syncTargetFor(jobID string) durability.SyncTarget {
	if app.dispatcher != nil {
		if j, ok := app.dispatcher.Job(jobID); ok {
			if _, err := j.Manifest(); err != nil {
				app.log.Debug("checkpoint skipped, job has no resident manifest",
					"job", jobID, "err", err)
			} else {
				return app.assembler.SyncTargetFor(jobID)
			}
		}
	}
	return nil
}

// checkpointJob runs one barrier for one job.
//
// Everything a barrier means lives in durability.Barrier; what lives here is
// when it happens. The per-job lock and the accumulator reset are the two
// pieces of that policy the barrier cannot own: it is not reentrant and it
// does not know what triggered it.
func (app *Application) checkpointJob(ctx context.Context, jobID string) bool {
	if app.barrier == nil {
		return true
	}
	mu := app.jobBarrierLock(jobID)
	defer app.releaseJobBarrierLock(jobID)
	mu.Lock()
	// --- Barrier serialised per job below this line ---

	// Three outcomes, not two. "The barrier ran and failed" and "no barrier
	// ran at all" are different facts, and folding the second into the first's
	// nil-error case is how a job on a dead mount came to report a fresh
	// last_barrier stamp every 30 seconds — the exact inversion of what R26
	// asks that figure to distinguish.
	//
	// The path is ordinary, not defensive, and it is worth naming precisely
	// because the obvious guess is wrong. Eviction does NOT produce a nil
	// target: syncTargetFor goes through Queue.SnapshotJob, which hydrates one
	// job's manifest from disk, so a merely paused job still has one. The
	// reachable cases are a job that left the queue between checkpointAll's
	// OpenJobIDs and this call — the assembler still holds handles for a job
	// the queue has dropped, so checkpointAll keeps listing it — and a
	// manifest that cannot be read at all.
	//
	// TestCheckpointJob_DoesNotStampABarrierThatNeverRan uses the first, and
	// asserts both halves of the fixture: no target, and the assembler still
	// listing the job.
	tgt := app.syncTargetFor(jobID)
	if tgt == nil {
		mu.Unlock()
		// --- No lock held below this line ---
		//
		// The accumulator is deliberately NOT reset. It measures a window
		// this call did not close, and zeroing it would report zero pending
		// bytes beside the stale timestamp above — two figures agreeing that
		// nothing is at risk, at the moment when everything written since the
		// last real barrier is.
		app.log.Debug("checkpoint skipped, no sync target for the job", "job", jobID)
		// UNSAFE to clear. No barrier ran, and the reachable cause — a job
		// that left the queue while the assembler still holds its handles —
		// is one where written articles may be sitting unacked (#417).
		return app.nothingAtRisk(jobID)
	}

	// A run over NO files claims nothing, and must not be read as a barrier.
	//
	// SyncTarget.Files answers nil when OpenFiles times out on a wedged mount,
	// deliberately, because the barrier has nothing useful to do with the
	// error. Run then iterates nothing, Commit returns early on an empty
	// slice, the ack is skipped for an empty set, and the nil that comes back
	// is indistinguishable from a barrier that fsynced everything. Stamping it
	// reported a fresh last_barrier every interval while nothing had reached
	// disk since the mount went away — the inversion R26 asks that figure to
	// prevent — and resetting the accumulator beside it reported zero bytes at
	// risk at the same moment.
	//
	// This costs a second Files() call on the healthy path, which is one
	// control message to a worker that is about to be asked again. On the
	// wedged path both are bounded by barrierOpTimeout, and by the per-job
	// deadline checkpointAll now imposes.
	// Asked of the assembler directly rather than through SyncTarget.Files,
	// which reports ANY error as "no files" because the barrier has nothing
	// useful to do with one (synctarget.go). Here the difference decides
	// whether a job's Emitted bits may be cleared, and a wedged mount that
	// times out is exactly the case that must not be read as "nothing to do"
	// — it is the case with the most written-but-unacked bytes. #417.
	//
	// finalizeCompletedFile asks the same question the same way and for the
	// same reason, after its own early-return guards.
	open, filesErr := app.assembler.OpenFiles(ctx, jobID)
	switch {
	case errors.Is(filesErr, assembler.ErrAssemblerStopped):
		// SAFE. The one error meaning "genuinely nothing" rather than
		// "unknown": the ordinary end of every process. Treating it as
		// unknown would hold the job's Emitted bits until a restart on every
		// clean shutdown, and an article whose data died with the old
		// downloader is never acked, so that stall would not self-clear.
		mu.Unlock()
		// --- No lock held below this line ---
		app.log.Debug("checkpoint skipped, the assembler has stopped", "job", jobID)
		return true
	case filesErr != nil:
		// UNSAFE. We do not know what the job holds, which is not the same as
		// knowing it holds nothing.
		mu.Unlock()
		// --- No lock held below this line ---
		//
		// The accumulator is deliberately NOT reset, for the reason the
		// nil-target branch above gives.
		app.log.Warn("checkpoint could not tell whether the job still holds open files; "+
			// Conditional, because the return is: nothingAtRisk answers true
			// for a job that has written nothing since its last successful
			// barrier, and such a job is cleared normally.
			"if it holds unacked written bytes, its emitted articles will not be "+
			"cleared by a reload",
			"job", jobID, "err", filesErr)
		return app.nothingAtRisk(jobID)
	}
	if len(open) == 0 {
		mu.Unlock()
		// --- No lock held below this line ---
		//
		// SAFE, and NOT because "nothing was written" — that is false. A drain
		// that closed its handles leaves written articles unacked with their
		// Emitted bits set (assembler.go's drainAndClose, filewriter.go:449
		// and :868).
		//
		// It is safe because no class that reaches here can be re-dispatched
		// with unacked bytes behind it. FOUR paths remove a job's files from
		// the open set — three found by `git grep -n 'delete(open'`
		// (CancelJob, CloseJobHandles, opClose), plus drainAndCloseAll at
		// worker exit, which that grep does not find because it discards the
		// whole set rather than deleting from it. Each is safe for its own
		// reason:
		//
		//   - CloseJobHandles: the job is already StatusVerifying, and
		//     ForEachUnfinishedArticle skips a PostProc job, so its Emitted
		//     bits cannot produce a re-fetch whether cleared or not.
		//   - CancelJob: the job is removed from the queue, so ClearAllEmitted
		//     never iterates it.
		//   - opClose, from CloseFile/FinalizeFile: the barrier acked those
		//     articles before the close.
		//   - drainAndCloseAll at worker exit: the assembler is stopping, so
		//     the arm above answers ErrAssemblerStopped and this one is not
		//     reached.
		//
		// One residual, and it is covered elsewhere rather than here: a file
		// finalized by an EARLIER checkpoint whose ack failed with
		// job.ErrNotResident can reach this exit with unacked articles.
		// noteNeedsSeed put that job on the stall list for replay when that
		// happened, which is the mechanism that resolves it.
		//
		// A job that simply never opened a file is safe trivially.
		//
		// The accumulator is deliberately NOT reset, for the reason the
		// nil-target branch above gives.
		app.log.Debug("checkpoint skipped, the job has no open files to sync", "job", jobID)
		return true
	}

	// Read WITHOUT clearing. The accumulator is retired by settleJobBytes on
	// the success path below, and by nothing else — a job whose barrier is
	// still in flight therefore stays visible to jobsAtRisk, which is what
	// stops a concurrent reload clearing its Emitted bits (#417).
	pending := app.pendingBytesFor(jobID)

	app.barrierRuns.Add(1)
	found, err := app.barrier.Run(ctx, jobID, tgt)
	mu.Unlock()
	// --- No lock held below this line ---

	// Reported after the unlock rather than under it. The lock is held across
	// the barrier's I/O by design — that is the serialisation — but a log
	// write is I/O of its own, and a slow handler would hold every other
	// checkpoint for this job behind a message about one that already failed.
	//
	// Post anomalies travel the same route for the same reason, and the reason
	// is I/O under a lock and nothing more. barrier-mu already nests q.mu on
	// every successful checkpoint, through AckDurable, so reporting under it
	// would introduce no new ordering — there is no deadlock argument here.
	app.reportPostAnomalies(jobID, found)
	if err != nil {
		// Nothing to put back: the accumulator was never cleared, so it still
		// describes the bytes at risk. A failed barrier leaves the figure
		// exactly where it was rather than dropping it to zero beside a
		// last_barrier that did not move.
		//
		// A failed ack is the one failure that DOES claim something. Run
		// commits the runs and then acks, so job.ErrNotResident means the
		// articles are on stable record while the live work set still calls
		// them Outstanding — and nothing replayed them, because this
		// failure never went through routeFault and so never put the job on
		// the stall list. retryFinalize treats the identical error as
		// recoverable and documents the replay; this path did not.
		if errors.Is(err, job.ErrNotResident) {
			app.log.Info("checkpoint recorded its durable runs but could not ack a non-resident job; "+
				"recorded for replay from the durability record", "job", jobID)
			app.noteNeedsSeed(jobID)
			// UNSAFE if anything is at risk. The articles are on stable
			// record, but the live work set has not been updated to say so —
			// precisely the state a clear would turn into a re-fetch.
			return app.nothingAtRisk(jobID)
		}
		// Everything else: the fault has already reached the job through
		// Stallable, and a failed barrier claims nothing — the prior committed
		// cache is intact and no article was acked.
		app.log.Warn("checkpoint barrier failed", "job", jobID, "err", err)
		// UNSAFE. The barrier claims nothing on this path, so anything written
		// since the last successful one is still unacked.
		return app.nothingAtRisk(jobID)
	}
	// Retired only now, by the run that actually earned it.
	app.settleJobBytes(jobID, pending)
	app.noteBarrierRun(jobID)
	return true
}

// noteBarrierRun stamps a job's last successful barrier (R26).
//
// Only a barrier that returned nil counts. The figure exists to tell a job
// that is checkpointing normally from one whose barriers have been failing
// since the mount went away, and stamping the attempt rather than the success
// would erase exactly that distinction.
func (app *Application) noteBarrierRun(jobID string) {
	app.barrierMu.Lock()
	defer app.barrierMu.Unlock()
	app.lastBarrier[jobID] = time.Now()
}

// hasBarrierStamp reports whether a job has ever had a barrier stamped.
//
// Presence rather than the time itself, because that is what R26 needs
// distinguishable: a zero time and "no barrier has ever succeeded" are the
// same value, and the whole point of the figure is to separate a job that is
// checkpointing normally from one whose barriers have been failing since the
// mount went away.
func (app *Application) hasBarrierStamp(jobID string) bool {
	app.barrierMu.Lock()
	defer app.barrierMu.Unlock()
	_, ok := app.lastBarrier[jobID]
	return ok
}

// settleJobBytes retires the bytes a SUCCESSFUL barrier made durable, and is
// the only thing that reduces a job's accumulator.
//
// It subtracts the figure the barrier read before it ran, rather than clearing
// the entry, and the difference is the whole point. An article written while
// the barrier was in flight belongs to the NEXT window — it may or may not have
// made it into this drain — so it must survive the settle. Clearing would
// discard exactly those bytes: the most recently written, and the least likely
// of all to be on disk.
//
// It replaced a take-before-the-run plus a restore-on-failure. That pair was
// correct about the arithmetic and wrong about the WINDOW: between the take and
// the restore the job had no entry at all, so jobsAtRisk could not name it, and
// a reload whose OpenJobIDs failed in that window cleared the Emitted bits of a
// job whose barrier then failed — #417 through a narrower door. Retiring the
// bytes only on success closes that by construction: there is no interval in
// which written-but-unacked bytes are invisible, because nothing removes them
// until they are neither.
//
// Called by the barrier itself, so the byte bound measures the window between
// barriers rather than the window between kicks.
func (app *Application) settleJobBytes(jobID string, n int64) {
	if n <= 0 {
		return
	}
	app.barrierMu.Lock()
	defer app.barrierMu.Unlock()
	// Non-positive rather than zero: settling more than the entry holds is not
	// reachable today (the figure came from this job's own accumulator, which
	// only grows between the read and here), but leaving a negative behind
	// would silently credit the next window.
	if remaining := app.jobBarrierBytes[jobID] - n; remaining > 0 {
		app.jobBarrierBytes[jobID] = remaining
		return
	}
	delete(app.jobBarrierBytes, jobID)
}

// pendingBytesFor reports how many bytes have been written for a job since its
// last barrier, without disturbing the accumulator.
func (app *Application) pendingBytesFor(jobID string) int64 {
	app.barrierMu.Lock()
	defer app.barrierMu.Unlock()
	return app.jobBarrierBytes[jobID]
}

// nothingAtRisk reports whether a job whose checkpoint did NOT succeed is
// nevertheless safe to clear, because it has written nothing since its last
// successful barrier.
//
// This is what keeps the withheld set as narrow as the hazard: the skip is per
// JOB while the hazard is per ARTICLE, so a job with one unacked article and a
// thousand untouched ones would withhold all thousand.
//
// It is worth being precise about what withholding costs now, because the
// original justification here named a class that no longer exists. That class
// was an article whose Emitted bit was set and whose result was then dropped by
// a cancelled emitResult — never written, never acked, and so never cleared by
// anything but a restart. emitResult clears the bit itself on that path now
// (dispatch.go), which is where the owner of that bit is, so withholding cannot
// strand it.
//
// What withholding still costs is delay for articles whose bytes ARE on disk
// awaiting a barrier — the drainAndClose class. Those are exactly the articles
// #417 is about, the delay is the point, and a later barrier releases them.
// This predicate keeps that delay off jobs that have nothing on disk to wait
// for, which is not rare: perJobShare divides one budget by the job count, so a
// queue with a few dozen open jobs gives each about 200ms and marks many of
// them unsafe on ordinary hardware.
//
// A job with an empty accumulator has nothing a clear could strand, whatever
// happened to its barrier. See jobsAtRisk for why the accumulator is the right
// source.
func (app *Application) nothingAtRisk(jobID string) bool {
	return app.pendingBytesFor(jobID) == 0
}

// jobsAtRisk names every job holding bytes written since its last SUCCESSFUL
// barrier — the exact set whose Emitted bits a reload must not clear (#417).
//
// The accumulator is the right source rather than a convenient one.
// onArticleWritten feeds it on every accepted write and its own doc says it
// "counts accepted bytes, not durable ones: it is measuring how much work is at
// risk between barriers". settleJobBytes is the only thing that reduces it, and
// only a successful barrier calls it — so an absent entry means a barrier
// really did make everything durable.
//
// That is also why this needs no in-flight case, and why it does not have to
// take the per-job barrier mutex to be correct. A job whose barrier is running
// right now still holds its entry; it is retired after the run succeeds, never
// before. An earlier design read-and-cleared before the run, which left a
// window where a job at risk had no entry at all — see settleJobBytes.
//
// Used where the sweep cannot enumerate jobs itself and so cannot ask
// checkpointJob about any of them.
func (app *Application) jobsAtRisk() map[string]struct{} {
	app.barrierMu.Lock()
	defer app.barrierMu.Unlock()
	if len(app.jobBarrierBytes) == 0 {
		return nil
	}
	at := make(map[string]struct{}, len(app.jobBarrierBytes))
	for jobID, n := range app.jobBarrierBytes {
		if n > 0 {
			at[jobID] = struct{}{}
		}
	}
	return at
}

// checkpointAll runs a barrier for every job holding an open file.
//
// The set comes from the assembler rather than from the queue, because "has an
// open file" is the assembler's fact and R8 bounds barrier cost by exactly
// that set: a barrier fsyncs open files, not every file a job will eventually
// produce. Deriving the set from job status instead would be a second
// representation of the same fact, free to drift (S5).
// It deliberately does NOT propagate the unprotected-jobs set its budgeted form
// returns. Its callers are the periodic sweep and the tests, and neither clears
// Emitted bits afterwards — only ReloadDownloader does, and it calls
// checkpointAllShare. Returning a value nothing reads would be an invitation to
// read it somewhere it does not apply.
func (app *Application) checkpointAll(ctx context.Context, perJob time.Duration) {
	app.checkpointAllWithBudget(ctx, func(int) time.Duration { return perJob })
}

// checkpointAllShare runs a barrier for every job holding an open file, giving
// each an EQUAL SHARE of one overall budget.
//
// This is the shutdown shape, and it exists because passing the same duration
// as both the sweep's context and each job's budget does not do what it looks
// like. context.WithTimeout cannot exceed its parent, so with a 10s sweep and a
// 10s per-job budget, a first job taking 9s leaves every job behind it with an
// already-expired context and an immediate failure — everything they had
// downloaded since their last barrier is re-fetched on the next start, which is
// the entire cost this checkpoint exists to avoid.
//
// The periodic sweep keeps a fixed per-job budget instead, deliberately: it has
// no overall deadline to divide, and one job's slow mount must not shrink
// every other job's budget on every tick.
func (app *Application) checkpointAllShare(ctx context.Context, total time.Duration) map[string]struct{} {
	return app.checkpointAllWithBudget(ctx, func(jobs int) time.Duration {
		return perJobShare(total, jobs)
	})
}

// perJobShare divides one overall budget among the jobs a sweep will visit.
func perJobShare(total time.Duration, jobs int) time.Duration {
	if jobs <= 1 {
		return total
	}
	return total / time.Duration(jobs)
}

// checkpointAllWithBudget is the shared loop. budget is asked once, after the
// job list is known, so a policy can divide by it.
func (app *Application) checkpointAllWithBudget(ctx context.Context, budget func(jobs int) time.Duration) map[string]struct{} {
	// The returned set names the jobs this sweep could NOT protect — those
	// whose checkpointJob reported unsafe. ReloadDownloader withholds their
	// Emitted bits from ClearAllEmitted (#417); every other caller ignores it,
	// which Go permits for a call statement, so nothing else needed changing.
	//
	// The two early returns below cannot ask checkpointJob about any job,
	// because neither reaches the loop. They answer from the barrier
	// accumulator instead, which names the at-risk jobs directly.
	//
	// A failed OpenJobIDs is NOT the transient listing error it looks like.
	// submit bounds that call with barrierOpTimeout, and the way it fails in
	// production is a worker parked in an fsync on a wedged mount — the state
	// with the MOST written-but-unacked bytes, and exactly what the per-job
	// arm below calls unsafe. Answering nil here would let one timed-out
	// listing clear every job's Emitted bits, with the two arms disagreeing
	// about identical evidence and the wider one winning.
	//
	// A nil barrier is unreachable in production, and nothing accumulates
	// without one (noteJobBytes is inert), so it answers the empty set.
	//
	// COVERAGE, stated so the question does not have to be re-derived: this
	// sweep visits OpenJobIDs — jobs holding an open file — while
	// ClearAllEmitted iterates every resident job, so the set is always a
	// subset. That is not an unclosed instance of #417. A resident job with no
	// open file either never wrote anything, or had its handles closed by
	// CloseJobHandles, which runs only on a job already at StatusVerifying;
	// ForEachUnfinishedArticle skips a PostProc job, so its Emitted bits
	// cannot produce a re-fetch whether they are cleared or not.
	if app.barrier == nil {
		return app.jobsAtRisk()
	}
	jobs, err := app.assembler.OpenJobIDs(ctx)
	if err != nil {
		app.log.Warn("checkpoint skipped, could not list jobs with open files; every job "+
			"holding unacked written bytes keeps its emitted bits", "err", err)
		return app.jobsAtRisk()
	}
	var unsafe map[string]struct{}
	perJob := budget(len(jobs))
	for _, jobID := range jobs {
		// Each job gets its own deadline, and that is what makes B4/R22 true
		// on this path rather than merely stated.
		//
		// The assembler's submit now applies barrierOpTimeout underneath
		// whatever the caller supplies, so no single operation is unbounded
		// either way. This deadline bounds the JOB — a barrier is a few dozen
		// operations, and a mount that is merely slow rather than wedged can
		// take longer than the whole cadence without any one of them expiring.
		//
		// The periodic caller used to have no deadline at all: runCheckpoint
		// is launched with the application's lifetime context, cancelled only
		// at shutdown. A worker parked in an fsync then blocked the barrier
		// for as long as the mount stayed down, and with it the single loop
		// that also owns stall re-evaluation and the queue save.
		//
		// PER JOB rather than per sweep, because a bound on the whole sweep
		// would let one wedged job consume the budget of every job behind it —
		// turning one bad mount into a queue-wide outage by a different route.
		jobCtx, cancel := context.WithTimeout(ctx, perJob)
		if app.jobCheckpointHook != nil {
			// Test seam: the budget a job is actually given is otherwise
			// unobservable, and it is the quantity the shutdown path got
			// wrong.
			app.jobCheckpointHook(jobCtx)
		}
		if !app.checkpointJob(jobCtx, jobID) {
			if unsafe == nil {
				unsafe = make(map[string]struct{})
			}
			unsafe[jobID] = struct{}{}
		}
		cancel()
	}
	return unsafe
}

// ErrNotFinalized reports that a completed file's bytes on disk are not known
// to be correct, so the file must not be treated as complete.
//
// It exists because "there was nothing to finalize" and "we could not find out
// whether there was anything to finalize" are different answers with the same
// shape, and only the caller can act on the difference.
var ErrNotFinalized = errors.New("app: completed file was not finalized")

// finalizeCompletedFile checkpoints a file whose parts have all arrived, trims
// it to its real extent, and hands its handle back to the assembler.
//
// This is the second half of the handoff assembler.finalizeFile starts. The
// assembler stops short of closing the file precisely so this can run: the
// truncate, the last drain's acks and the size/mtime stamp all need the handle
// it owns.
//
// The bound the trim uses is the highest end offset among the file's DURABLE
// articles, which is neither this run's high-water mark (#342, #350: a resumed
// file would be cut back to what this process happened to fetch) nor the
// gapless prefix (which stops at the first permanently failed article and
// would destroy the very blocks par2 repairs from). Barrier.FinalizeFile owns
// that derivation; this function owns getting it called at all.
//
// # What the result means
//
// nil means the file's bytes on disk are as correct as this process can make
// them: either it was finalized, or there was legitimately nothing to finalize
// (the assembler has stopped, or the job has left the queue — both ordinary,
// and in both cases nothing downstream will act on the file either).
//
// ErrNotFinalized means the opposite, and the caller MUST NOT treat the file
// as complete. It used to return nothing at all, and the caller proceeded
// identically either way — straight into MarkFileComplete, DirectUnpack, job
// finalization and post-processing. That made a barrierOpTimeout on a wedged
// mount indistinguishable from an ordinary shutdown: the file was never
// trimmed, its last drain was never acked, and it was then closed for good and
// shipped with pre-allocation's trailing zeros intact, which par2 reports as
// damage on a download that was perfectly healthy. That is #350 arriving by a
// different route, and it was silent.
//
// The handle is RETAINED on the failing path, reversing the earlier decision
// to close it there. That decision rested on a premise that no longer holds:
// "no path re-triggers a finalize for a file whose parts have all arrived".
// Application.reevaluateStall is now that path, and every operation it needs —
// Drain, Sync, Truncate, Stat — goes through the handle the assembler owns.
// Closing here left the stall unable to clear for the rest of the process,
// which is the L2 violation this reversal exists to remove.
//
// # What the retained set is actually bounded by
//
// Not the concurrently-open set, which is what the close bounded. This is a
// CUMULATIVE set: one fd per completed-but-unfinalized file, held until a
// retry succeeds. Its ceiling is the files that had already completed, or were
// already queued on internalFileComplete, when the fault hit — the open-file
// set plus that channel's 128-entry buffer.
//
// # The bound holds WHILE THE JOB IS PARKED, and a user Resume is the boundary
//
// reevaluateStall does not resume a job until every interrupted finalize has
// landed, so on the automatic path no further file of that job can complete
// and add to the set. An earlier draft resumed first and returned at the first
// failing file, which let each interval complete more files, retry only one of
// them, and retain the rest — an unbounded climb toward EMFILE on a mount that
// stays broken.
//
// A USER Resume is outside that guarantee, and deliberately so: the API's
// queue resume handlers unpause the job and THEN ask for a re-evaluation
// (internal/api/queue.go, queueResumeJobs and queueResumeAll), because a user
// who has cleared the condition is entitled to have their job run. If the
// condition has NOT cleared, that job downloads, completes more files, fails
// each finalize, and retains each handle until the next re-evaluation parks it
// again. The set therefore grows by at most what one re-evaluation interval's
// worth of downloading can complete, per user Resume — not without limit, and
// not silently: each of those files raises its own routed fault.
//
// post-processing's unlink cannot become an NFS silly-rename because a parked
// job does not reach post-processing. CancelJob, CloseJobHandles and the
// assembler's own shutdown drain all still release the handles.
func (app *Application) finalizeCompletedFile(ctx context.Context, jobID string, fileIdx int) (err error) {
	defer func() {
		if err != nil {
			// The handle stays open on the failing path, so the retry in
			// reevaluateStall has something to finalize. Drain, Sync,
			// Truncate and Stat all need it, and nothing reopens a file the
			// assembler has tombstoned.
			return
		}
		if cerr := app.assembler.CloseFile(ctx, jobID, int32(fileIdx)); cerr != nil { //nolint:gosec // G115: file counts are far below int32
			app.log.Debug("close completed file handle", "job", jobID, "fileidx", fileIdx, "err", cerr)
		}
	}()
	if app.barrier == nil {
		return nil
	}
	tgt := app.syncTargetFor(jobID)
	if tgt == nil {
		// A nil target is a third outcome dressed as success, the same shape
		// checkpointJob's stamp was — so it needs an argument rather than an
		// assurance.
		//
		// It is safe HERE, on the first attempt, because a nil target implies
		// the completion below is refused anyway. syncTargetFor is the WEAKER
		// requirement of the two: it is satisfied by any job whose manifest
		// can be hydrated, including a paused one, while MarkFileComplete
		// needs the LIVE job resident. So nil means the job has left the
		// queue or its manifest cannot be read, and MarkFileComplete answers
		// dispatch.ErrNotFound or job.ErrNotResident to both. Nothing downstream acts on
		// the file.
		//
		// It is NOT safe on a retry, where the completion is queued behind
		// this call and can be delivered on a later cycle once the job is
		// resident again — by then the file would be recorded finalizeDone
		// without ever having been trimmed. retryFinalize guards it there.
		return nil
	}
	trunc, ok := tgt.(durability.Truncator)
	if !ok {
		// Unreachable with the real adapter, which implements Truncate, and
		// reported rather than assumed: without the trim a completed file
		// keeps pre-allocation's trailing zeros and par2 reports a healthy
		// download as damaged.
		return fmt.Errorf("%w: job %s file %d: the assembler sync target cannot truncate",
			ErrNotFinalized, jobID, fileIdx)
	}

	// Ask the assembler directly rather than through SyncTarget.Files, which
	// reports an error as "no files" because the barrier has nothing useful to
	// do with one. Here the difference decides whether a file ships: a stopped
	// assembler means there is nothing to finalize, while a timeout means we
	// do not know, and only one of those may proceed.
	open, err := app.assembler.OpenFiles(ctx, jobID)
	switch {
	case errors.Is(err, assembler.ErrAssemblerStopped):
		// The ordinary end of every process. watchCompletions drains its
		// pending completions after the assembler has stopped, so every
		// completion still in flight arrives here — and each is a file the
		// assembler already drained, fsynced and closed on its way out.
		app.log.Debug("completed file arrived after the assembler stopped; nothing to finalize",
			"job", jobID, "fileidx", fileIdx)
		return nil
	case err != nil:
		return fmt.Errorf("%w: job %s file %d: cannot tell whether it is still open: %w",
			ErrNotFinalized, jobID, fileIdx, err)
	}
	if !slices.Contains(open, int32(fileIdx)) { //nolint:gosec // G115: file counts are far below int32
		// Some other path closed it first — CancelJob, or CloseJobHandles on
		// a job entering post-processing. Both are deliberate and both drain
		// and sync before closing.
		app.log.Debug("completed file is no longer open; nothing to finalize",
			"job", jobID, "fileidx", fileIdx)
		return nil
	}

	mu := app.jobBarrierLock(jobID)
	defer app.releaseJobBarrierLock(jobID)
	mu.Lock()
	// --- Barrier serialised per job below this line ---
	found, err := app.barrier.FinalizeFile(ctx, jobID, int32(fileIdx), trunc) //nolint:gosec // G115: file counts are far below int32
	mu.Unlock()
	// --- No lock held below this line ---

	// Below the unlock for the same reason the error report is, and reported
	// even though the file is about to be called finalized: it IS finalized,
	// and it is also damaged. #410 already withheld its whole-file CRC, which
	// sends it to par2; this is what tells the user why.
	app.reportPostAnomalies(jobID, found)
	if err != nil {
		// Reported by the caller, not here: the lock spans the barrier's I/O
		// by design, and a log write is I/O of its own with no business
		// extending that span.
		return fmt.Errorf("%w: job %s file %d: %w", ErrNotFinalized, jobID, fileIdx, err)
	}
	// The assembled CRC is NOT recorded here. It moved to
	// completeFinalizedFile, which every completion path reaches and this one
	// does not: a stall recovery whose ack found the job non-resident returns
	// above, and the startup repair never calls this function at all. See that
	// function for the two paths this placement used to miss.
	return nil
}

// recordAssembledCRC copies the finalized file's whole-file CRC onto the queue,
// where the quickcheck STAGE and on-demand par2 read it.
//
// Both reach it through par2.AssessWithOptions, which compares a par2 set's
// recorded CRC32 against the one this download produced.
//
// This used to warn which function that was NOT — par2.QuickCheck, filename
// relocation computing its own CRC from a path via tryMatchCRC32File. Neither
// exists: #494 made Assess the one call that identifies, verifies and reports
// the relocations that follow, so there is no longer a second CRC to confuse
// this one with. Only the post-processing STAGE still carries the name.
//
// With none recorded both read NoCRC and take the conservative branch. On the
// on-demand side par2Verdict returns needsRecovery, and its caller fetches
// every recovery volume even for a bit-perfect download; on the stage side the
// verdict cannot be Clean, so stage_repair.go runs the full par2 verify+repair
// subprocess. That is safe but it is not free, and it was the state of every
// download.
//
// # This function does not decide whether the CRC is publishable
//
// It hands the whole record over and lets Queue.SetFileCRC32FromRuns decide.
// The decision needs the file's article range, which lives on the resident
// manifest that the queue owns and this package deliberately does not reach
// into (docs/queue-lifecycle.md), and — more to the point — a setter that took
// a bare uint32 would have no way to refuse a wrong one. The predicate, and
// the argument for its exact shape, are at the gatekeeper.
//
// What is worth knowing HERE is why the value is trustworthy at all. It is a
// QUERY rather than a walk: a run's CRC32 is combined left-to-right over the
// articles as they join it, so a file that collapses to a single run at offset
// 0 carries the whole-file CRC already computed, on stable storage, with no
// prefix state to maintain.
//
// That source is what #349's combine lacked: the assembler could only see the
// articles THIS run fetched, so a resumed file's parts did not tile and the
// figure described a fragment. Runs persist, so they account for every article
// of the file whichever process fetched it.
//
// Best effort by design. A missing CRC costs a full verify, which is exactly
// today's behaviour, so a failure here must not fail the finalize that has
// already committed the runs and acked the articles.
func (app *Application) recordAssembledCRC(ctx context.Context, jobID string, fileIdx int) {
	if app.runs == nil {
		return
	}
	runs, err := app.runs.ForFile(ctx, jobID, int32(fileIdx)) //nolint:gosec // G115: file counts are far below int32
	if err != nil {
		app.log.Debug("load the durable runs to record the assembled CRC", "job", jobID, "err", err)
		return
	}
	if app.dispatcher != nil {
		if j, ok := app.dispatcher.Job(jobID); ok {
			_, _ = j.SetFileCRC32FromRuns(fileIdx, runs)
			if app.checkpointer != nil {
				app.checkpointer.Mark(j)
			}
		}
	}
}

// runCheckpoint is R6's cadence: a barrier per job on the lesser of a time
// bound and a byte bound, and a queue save after it.
//
// The queue save follows the barrier rather than running on its own timer
// because the barrier is what produces something worth saving — and what it
// produces is the durable_runs commit, which is already on stable storage
// before the ack. Saving first would persist a snapshot taken before that
// commit, which is stale by construction.
//
// What the save carries is therefore the FILE-level state the record does not
// hold: Complete, the assembled CRC, the fetch policy. Article resolution
// survives a crash without it, because the startup sweep replays the runs.
// This paragraph used to say a crash re-fetches unsaved acked articles, which
// was true of the per-article bitmap this design deleted and is not true now;
// what a crash between the commit and the save actually strands is a file's
// Complete flag (docs/durability-contract.md, limitation 6).
//
// Two of the other three triggers R6 names are not here, because they are
// events rather than a cadence: file completion goes through
// finalizeCompletedFile, and clean shutdown through shutdownCheckpoint, which
// stopWorkers calls between stopping the downloader and stopping the assembler.
//
// The third, PAUSE, is NOT IMPLEMENTED anywhere. No code path runs a barrier
// when a job is paused; a paused job stops writing and its buffered bytes wait
// for the next interval tick or for shutdown. Said plainly because an earlier
// version of this comment folded pause into "Shutdown's final pass", which
// reads as coverage and is not.
func (app *Application) runCheckpoint(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// R19's second cadence, on this goroutine rather than its own. A
	// re-evaluation retries a finalize, which takes the same per-job barrier
	// lock a checkpoint does; running the two from one loop means they are
	// serialised by construction rather than by that lock.
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
		case <-ticker.C:
			// The cadence is the bound: a job whose checkpoint cannot
			// finish within one interval is already failing to keep up, so
			// spending longer on it only delays every job behind it.
			app.checkpointAll(ctx, interval)
		case jobID := <-app.barrierKick:
			app.checkpointJob(ctx, jobID)
		case <-stallTicker.C:
			app.reevaluateStalls(ctx)
		case <-app.stallKick:
			app.reevaluateStalls(ctx)
		}
		app.saveQueueIfDirty()
	}
}

// saveQueueIfDirty persists the queue when something changed.
func (app *Application) saveQueueIfDirty() {
	if app.checkpointer != nil {
		_ = app.checkpointer.Flush(context.Background())
	}
}

// shutdownCheckpoint is R6's clean-shutdown trigger.
//
// It runs after the downloader has stopped and before the assembler does,
// which is the only window where both halves hold: no new article can arrive,
// and the file handles the barrier needs still exist. Running it after the
// assembler stopped would find nothing open; running it before the downloader
// stopped would leave whatever arrived in between unacked.
//
// Without it, everything downloaded since the last barrier is re-fetched on
// the next start — up to a full checkpoint window thrown away on every
// deliberate restart, which is the cost B1 bounds for a crash and nobody
// should pay for a clean stop.
func (app *Application) shutdownCheckpoint() {
	// Test seam. It exists because the ORDERING this function sits in is what
	// several rules depend on — the process must already be marked stopping
	// when the faults this raises are routed — and "true after Shutdown
	// returns" holds under a store placed anywhere, including after the
	// barrier it is meant to cover.
	if app.checkpointHook != nil {
		app.checkpointHook()
	}
	if app.barrier == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownCheckpointTimeout)
	defer cancel()
	// A SHARE of the budget each, not the whole of it. See checkpointAllShare:
	// passing the same duration for both collapses the per-job isolation this
	// function's own doc promises, and the jobs behind the first slow one pay
	// exactly the re-fetch cost the paragraph above says nobody should pay for
	// a clean stop.
	app.checkpointAllShare(ctx, shutdownCheckpointTimeout)
	app.saveQueueIfDirty()
}

// dropJobAlreadyInHistory removes a queue job that has already been filed in
// history, reporting whether it did.
//
// Reached at startup by a job that crashed between MoveToHistory and the queue
// removal that follows it. The queue row is a duplicate of an entry that is
// already the record.
//
// The durability rows go with it, under the same rule the ordinary transition
// applies in finalizeJob: unless the entry is FAILED, in which case a retry
// reuses them to bound FinalizeFile's truncate to the whole partial file
// rather than to the few articles that run re-fetches.
//
// This used to remove the queue row and stop. The history entry it fetched was
// discarded — the call read `_, err := ...Get(...)` — so the rule could not be
// applied, and durable_runs and failed_articles stayed behind. They are keyed
// by job ID with no foreign key to jobs. SQLiteStore.Prune now sweeps rows whose
// job is in neither the queue nor history-as-FAILED, so they no longer survive
// for the life of the installation -- but Prune is a backstop that runs on a
// queue save, not a substitute for removing them on the way out.
func (app *Application) dropJobAlreadyInHistory(ctx context.Context, jobID string) bool {
	dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
	entry, err := app.historyRepo.Get(dbCtx, jobID)
	dbCancel()
	if err != nil {
		if !errors.Is(err, history.ErrNotFound) {
			app.log.Error("failed to check history for job", "jobID", jobID, "err", err)
		}
		return false
	}
	app.log.Info("found completed job in history but still in queue, removing", "jobID", jobID)
	if app.dispatcher != nil {
		if rmErr := app.dispatcher.Remove(ctx, jobID); rmErr != nil {
			app.log.Error("failed to remove duplicate job from dispatcher", "jobID", jobID, "err", rmErr)
		}
	}
	manifestPath := filepath.Join(app.config.GetGeneral().AdminDir, "queue", "manifests", jobID+".json.gz")
	if rmErr := os.Remove(manifestPath); rmErr != nil && !os.IsNotExist(rmErr) {
		app.log.Debug("could not unlink manifest for duplicate job", "jobID", jobID, "err", rmErr)
	}
	if entry != nil && entry.Status == string(constants.StatusFailed) {
		return true
	}
	delCtx, delCancel := context.WithTimeout(ctx, 5*time.Second)
	app.deleteJobDurability(delCtx, jobID)
	delCancel()
	return true
}

// deleteJobDurability drops a DEPARTED job's durable runs and failed-article
// rows, logging a failure rather than reporting it.
//
// Both are keyed by job ID with no foreign key to the queue, so nothing
// removes them implicitly. SQLiteStore.Prune sweeps what escapes this call --
// the crash window between a job leaving `jobs` and this running -- but it is
// a backstop on a queue save, and a job that finished or was deleted must not
// wait for one.
//
// Swallowing the error is right HERE and wrong for a retry, which is why the
// two have separate entry points over one implementation. For a departed job
// the rows are garbage: leaving them costs disk until Prune runs, and there is
// no caller left to tell. A retry is the opposite case -- see
// dropJobDurability.
func (app *Application) deleteJobDurability(ctx context.Context, jobID string) {
	if app.historyRepo != nil && app.historyRepo.DB() != nil {
		_, _ = app.historyRepo.DB().ExecContext(ctx, "DELETE FROM job_files WHERE job_id = ?", jobID)
	}
	if err := app.dropJobDurability(ctx, jobID); err != nil {
		app.log.Warn("delete durability rows for a departed job", "job", jobID, "err", err)
	}
}

// dropJobDurability is the same deletion for a job that is coming BACK, where
// a failure must stop the caller.
//
// RetryHistoryJob calls this when the re-parsed manifest changed shape, so the
// retained rows describe articles that are no longer at those indices. They are
// not garbage there -- they are about to be read, and a stale row bounds
// FinalizeFile's truncate, which silently destroys the part of the partial file
// beyond it (#422). That is the exact harm this PR exists to prevent, so a
// cleanup that fails must abort the retry rather than proceed without it.
//
// Prune is no backstop for that case. Its sweep deliberately skips any job_id
// still present in `jobs`, and a retry is re-added to `jobs` moments later --
// so a row that survives this call survives for the life of the job.
//
// Both deletions are attempted even if the first fails, and both errors are
// joined: removing one of the two tables is still progress, and a caller
// deciding whether to abort is better served by the whole picture than by
// whichever failure came first.
//
// The two tables are durable_runs and failed_articles, and they are deleted
// through different owners on purpose. durability.RunStore owns the first and
// is the only thing that INSERTS OR AMENDS a run's content; queue.Store owns
// the second, which is why a job's failed articles are dropped through the
// queue's own entry point rather than by reaching into the table from here.
//
// The write bound is on content, not on deletion, and the distinction is not
// pedantry: durable_runs rows are deleted from five places outside the
// barrier's own merge — durability.Resumer, RunStore.DeleteJob (below),
// queue.SQLiteStore.removeCorrupt and pruneDurabilityRows, and
// history.Repository.delete. (A sixth, Commit's deleteRows, is part of the
// merge's read-modify-write rather than a separate deleter.) Content-only is
// still exactly the property the trust argument needs — nothing can make the
// record ASSERT bytes an fsync did not cover — and unlike "nothing else writes
// it", it is true.
func (app *Application) dropJobDurability(ctx context.Context, jobID string) error {
	var errs []error
	if app.runs != nil {
		if err := app.runs.DeleteJob(ctx, jobID); err != nil {
			errs = append(errs, fmt.Errorf("durable runs: %w", err))
		}
	}
	if app.historyRepo != nil && app.historyRepo.DB() != nil {
		if _, err := app.historyRepo.DB().ExecContext(ctx, `DELETE FROM failed_articles WHERE job_id = ?`, jobID); err != nil {
			errs = append(errs, fmt.Errorf("failed articles: %w", err))
		}
	}
	return errors.Join(errs...)
}

var _ durability.Stallable = (*Application)(nil)

// checkpointSettings resolves the two bounds from config, substituting the
// defaults for unset or nonsensical values.
//
// Neither can be disabled, and it is worth stating the reason accurately
// because the obvious version of it is wrong.
//
// A barrier is the only thing that RECORDS a downloaded article, and the only
// thing that acks one while the job is running. With checkpoints off a job
// makes no visible progress and holds every article Outstanding until it
// stops — and, unlike before, that is also exactly what a restart finds.
//
// The reason is worth stating plainly because it changed. There used to be a
// second, unordered record of every decoded article, so a resume could re-read
// the file and prove articles the barrier had never claimed. That record is
// gone: nothing is written before the fsync, so what no barrier recorded was
// never recorded at all and is genuinely re-fetched. Turning checkpoints off
// no longer trades a fast start for a slow one; it trades a bounded fsync
// cadence for re-downloading everything since the last one.
func checkpointSettings(interval time.Duration, bytes int64) (resolvedInterval time.Duration, resolvedBytes int64) {
	if interval <= 0 {
		interval = defaultCheckpointInterval
	}
	if bytes <= 0 {
		bytes = defaultCheckpointBytes
	}
	return interval, bytes
}

// filePathFor resolves a job file's on-disk path for a surfaced reason, or ""
// when it cannot be resolved.
//
// Diagnostic only; nothing may branch on it. It reads the pipeline's resolved
// FileInfo — the same value the assembler opened the file with — because a
// stall reason that names no file tells a user their download halted without
// telling them which volume or which mount to look at (R27).
//
// The error is discarded rather than branched on, deliberately: resolveFileInfo
// returns a zero FileInfo alongside it, so an `if err != nil { return "" }`
// guard would be a branch whose two arms produce the same value — untestable by
// construction, and the reason a test written against it had two assertions
// that could not fail.
func (app *Application) filePathFor(jobID string, fileIdx int) string {
	info, _ := app.pipeline.resolveFileInfo(jobID, fileIdx)
	return info.Path
}

// routeFinalizeFailure surfaces a failed finalize to the job, routes it only if
// nothing has routed it already, and records the file for retry.
//
// The record is the half concern 8 was missing. The assembler reports a file
// complete exactly once, so a failed finalize is the end of the road for that
// file unless something remembers it — and without that, the job stayed parked
// after the mount came back, which L2 calls a defect rather than a wait. Only
// a retryable fault is recorded: a permanent one took the job to Fail, which
// carries it into history rather than leaving it to recover.
//
// Barrier.routeFault dispatches every storage fault it meets per A1 — Fail for
// permanent, Stall for retryable — and then RETURNS that same fault as its
// error, so a caller that ignores Stallable still cannot mistake a fault for
// success. Re-classifying that returned error and stalling again is therefore
// not belt and braces; it is a second, wrong dispatch that overwrites the first.
//
// The observed damage: a permanent fault correctly reached Fail, which set the
// job's reason to "Failed: … read-only file system", and the unconditional
// Stall that followed replaced it with "Stalled: …" — presenting a condition
// that cannot clear as one the operator should wait out, and destroying the
// actionable reason on the way. The job's status and its warning ended up
// disagreeing with each other.
//
// So: if a *storagefault.Fault is anywhere in the chain, the barrier has
// already decided and acted, and there is nothing left to do but stop the
// completion. Everything else — an OpenFiles timeout, a target that cannot
// truncate, a failed commit — never reached routeFault and would otherwise
// halt the job with no reason attached at all.
func (app *Application) routeFinalizeFailure(jobID string, fileIdx int, path string, err error) {
	app.log.Error("completed file was not finalized; the job is halted rather than "+
		"shipping a file whose bytes are not known to be correct",
		"job", jobID, "fileidx", fileIdx, "err", err)

	// Asked of the MARKER, not of the error's shape. The test used to be "the
	// chain contains a *storagefault.Fault", which read every fault as routed
	// — including the one the SyncTarget boundary mints when the worker does
	// not answer, which nothing has routed. That one was then swallowed, and
	// the job carried on with a file that was never trimmed.
	if errors.Is(err, durability.ErrFaultRouted) {
		routed, _ := errors.AsType[*storagefault.Fault](err)
		app.log.Debug("the fault was already routed by the barrier; not routing it again",
			"job", jobID, "permanent", routed != nil && routed.Permanent)
		if routed == nil || !routed.Permanent {
			app.notePendingFinalize(jobID, fileIdx)
		}
		return
	}
	//
	// A non-resident job is a queue-residency condition. retryFinalize already
	// treats this as landed and says why — the runs are recorded, so the
	// articles are replayed from the record once the job resumes — and the
	// first attempt has no reason to answer differently.
	if errors.Is(err, job.ErrNotResident) {
		app.log.Debug("finalize recorded its durable runs but could not ack a non-resident job; "+
			"the articles are replayed from the record after the resume",
			"job", jobID, "fileidx", fileIdx)
		app.notePendingFinalize(jobID, fileIdx)
		return
	}
	// Not a storage condition, so the job is not parked for it: a deliberate
	// close, a stopped assembler, or a caller that stopped waiting. Parking a
	// job here named a disk that did not fail and offered an operator action
	// that does not exist (A1). The finalize is still owed, so it is recorded
	// for the retry that the next re-evaluation runs.
	if errors.Is(err, durability.ErrTargetUnavailable) {
		app.log.Info("completed file was not finalized, for a reason that is not a storage "+
			"condition; the job is not parked and the finalize is retried",
			"job", jobID, "fileidx", fileIdx, "err", err)
		app.notePendingFinalize(jobID, fileIdx)
		return
	}
	// A fault the target classified keeps its own op and path. Relabelling it
	// "finalize" against this file discarded which syscall actually failed,
	// which is the whole of what makes the reason actionable (R27).
	f, ok := errors.AsType[*storagefault.Fault](err)
	if !ok {
		f = storagefault.Classify("finalize", path, err)
	}
	if f.Path == "" {
		f.Path = path
	}
	app.Stall(jobID, f)
	if !f.Permanent {
		app.notePendingFinalize(jobID, fileIdx)
	}
}
