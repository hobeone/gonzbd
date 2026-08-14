package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// manifestArticleMap answers the two manifest questions the assembler cannot:
// how many articles a file holds, and where a global article index sits
// within it.
//
// The assembler is fed one article at a time and never sees the job's
// manifest, so any answer it invented would be a guess — and the barrier
// places durable bits by these ordinals, where a wrong one marks the wrong
// article durable and costs a silently short file. The queue owns the
// manifest, so the answer comes from here.
//
// queue.Manifest.FileRange indexes the manifest's slices directly and panics
// on an out-of-range file, so every method bounds fileIdx against NumFiles
// first. A file index the manifest does not have is a bookkeeping defect, and
// the honest answers (0 / false) make the barrier refuse to build an extent
// rather than crash the process.
type manifestArticleMap struct{ m *queue.Manifest }

func (a manifestArticleMap) rangeOf(fileIdx int32) (lo, hi int, ok bool) {
	i := int(fileIdx)
	if a.m == nil || i < 0 || i >= a.m.NumFiles() {
		return 0, 0, false
	}
	lo, hi = a.m.FileRange(i)
	return lo, hi, true
}

func (a manifestArticleMap) ArticleCount(fileIdx int32) int {
	lo, hi, ok := a.rangeOf(fileIdx)
	if !ok {
		return 0
	}
	return hi - lo
}

func (a manifestArticleMap) FileLocalOrdinal(fileIdx, artIdx int32) (int, bool) {
	lo, hi, ok := a.rangeOf(fileIdx)
	if !ok {
		return 0, false
	}
	i := int(artIdx)
	if i < lo || i >= hi {
		return 0, false
	}
	return i - lo, true
}

// handleWriteFault is the assembler's Options.OnWriteFault, and it does the two
// things a failed write needs that the assembler cannot do for itself.
//
// First it returns the article to Outstanding. The write did not happen, so
// the article must be fetched again — but its Emitted bit is still set from
// dispatch, and ForEachUnfinishedArticle skips a set Emitted bit. Nothing else
// clears it on this path: no Drain reports the article, no AckPermanentFailure
// names it, and eviction keeps job.progress, so pause and resume do not clear
// it either. Left alone it is stranded for the life of the process, at any
// residency, which is why this is not merely tidy.
//
// Then it routes the fault, on the same rule the barrier uses (R18/A1): a
// permanent condition fails the job, anything else stalls it. The article is
// never marked failed, because a storage fault says nothing about whether the
// article is available on any server.
//
// Runs on the assembler's worker goroutine, so it must not block on it: both
// calls take the queue lock only.
func (app *Application) handleWriteFault(jobID string, _ int, artIdx int32, f *storagefault.Fault) {
	if err := app.queue.ClearArticleEmittedByIdx(jobID, artIdx); err != nil {
		// A job that has left the queue has nothing to re-dispatch, so this is
		// ordinary rather than a defect. It is still not silent (A2).
		app.log.Debug("clear the emitted bit after a failed write",
			"job", jobID, "artidx", artIdx, "err", err)
	}
	if f.Permanent {
		app.Fail(jobID, f)
		return
	}
	app.Stall(jobID, f)
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
// The test is the APPLICATION's context, not the error. A wedged mount
// produces the identical context.DeadlineExceeded through barrierOpTimeout,
// and that one must still park the job with a reason — otherwise it sits at
// 99% with nothing surfaced, which A2 forbids. The two are distinguishable
// only by whether we are stopping, so that is what is asked.
func (app *Application) Stall(jobID string, f *storagefault.Fault) {
	if app.ctx != nil && app.ctx.Err() != nil {
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
	if err := app.queue.Pause(jobID); err != nil {
		app.log.Warn("stall: could not pause the job", "job", jobID, "err", err)
	}
	if err := app.queue.SetWarning(jobID, reason); err != nil {
		// Without the warning the job is paused for no visible reason, which
		// is precisely the unactionable halt R27 forbids — so this is logged
		// at Warn rather than swallowed.
		app.log.Warn("stall: could not surface the reason", "job", jobID, "err", err)
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
	if err := app.queue.SetWarning(jobID, reason); err != nil {
		app.log.Warn("fail: could not surface the reason", "job", jobID, "err", err)
	}
	app.maybeFinalize(jobID, reason)
}

// jobBarrierLock returns the mutex serialising barrier work for one job.
//
// Barrier.Run holds no lock of its own — it does I/O throughout, and this
// project bans I/O under a lock — so the cadence owner has to guarantee at
// most one barrier in flight per job. Two concurrent barriers over one job
// would interleave the read-modify-write of its FileExtent: both load the
// same stored bitmap, each adds its own bits, and the second commit
// overwrites the first, so the committed cache describes neither run and the
// articles the loser acked are durable with no bit to say so.
//
// The lock is per job, not global, because a barrier is a few dozen fsyncs
// and one job's slow mount must not park every other job's checkpoint.
//
// FinalizeFile takes it too. It is a barrier by another name — same
// buildExtent, same ExtentStore.Commit — so it races Run for exactly the same
// reason.
func (app *Application) jobBarrierLock(jobID string) *sync.Mutex {
	app.barrierMu.Lock()
	defer app.barrierMu.Unlock()
	mu, ok := app.jobBarrierMu[jobID]
	if !ok {
		mu = &sync.Mutex{}
		app.jobBarrierMu[jobID] = mu
	}
	return mu
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
	delete(app.jobBarrierMu, jobID)
	delete(app.jobBarrierBytes, jobID)
	delete(app.lastBarrier, jobID)
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

// resetJobBytes zeroes a job's accumulator. Called by the barrier itself, so
// the byte bound measures the window between barriers rather than the window
// between kicks.
func (app *Application) resetJobBytes(jobID string) {
	app.barrierMu.Lock()
	defer app.barrierMu.Unlock()
	delete(app.jobBarrierBytes, jobID)
}

// syncTargetFor builds the barrier's view of one job's open files, or nil when
// the job has no resident manifest.
//
// Nil rather than a target with a nil ArticleMap: the adapter answers
// "unknown" for every ordinal in that case, which makes the barrier refuse the
// job loudly. That refusal is correct and is deliberately not exercised here —
// a job whose manifest has been evicted is an ordinary event, not a defect,
// and it has nothing open to checkpoint anyway.
//
//nolint:ireturn // the barrier takes this interface; there is no concrete type to return
func (app *Application) syncTargetFor(jobID string) durability.SyncTarget {
	snap := app.queue.SnapshotJob(jobID)
	if snap == nil {
		return nil
	}
	m, err := snap.Manifest()
	if err != nil {
		app.log.Debug("checkpoint skipped, job has no resident manifest",
			"job", jobID, "err", err)
		return nil
	}
	return app.assembler.SyncTargetFor(jobID, manifestArticleMap{m: m})
}

// checkpointJob runs one barrier for one job.
//
// Everything a barrier means lives in durability.Barrier; what lives here is
// when it happens. The per-job lock and the accumulator reset are the two
// pieces of that policy the barrier cannot own: it is not reentrant and it
// does not know what triggered it.
func (app *Application) checkpointJob(ctx context.Context, jobID string) {
	if app.barrier == nil {
		return
	}
	mu := app.jobBarrierLock(jobID)
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
		return
	}

	// Reset before the run, not after. An article written while the barrier
	// is in flight belongs to the NEXT window: it may or may not have made
	// it into this drain, and charging it to the window that just closed
	// would let it go uncounted until the following one.
	app.resetJobBytes(jobID)

	app.barrierRuns.Add(1)
	err := app.barrier.Run(ctx, jobID, tgt)
	mu.Unlock()
	// --- No lock held below this line ---

	// Reported after the unlock rather than under it. The lock is held across
	// the barrier's I/O by design — that is the serialisation — but a log
	// write is I/O of its own, and a slow handler would hold every other
	// checkpoint for this job behind a message about one that already failed.
	if err != nil {
		// A failed ack is the one failure that DOES claim something. Run
		// commits Class B and then acks, so ErrJobNotResident means the bits
		// are on stable record while the live work set still calls those
		// articles Outstanding — and nothing replayed them, because this
		// failure never went through routeFault and so never put the job on
		// the stall list. retryFinalize treats the identical error as
		// recoverable and documents the replay; this path did not.
		if errors.Is(err, queue.ErrJobNotResident) {
			app.log.Info("checkpoint committed its extents but could not ack a non-resident job; "+
				"recorded for replay from Class B", "job", jobID)
			app.noteNeedsSeed(jobID)
			return
		}
		// Everything else: the fault has already reached the job through
		// Stallable, and a failed barrier claims nothing — the prior committed
		// cache is intact and no article was acked.
		app.log.Warn("checkpoint barrier failed", "job", jobID, "err", err)
		return
	}
	app.noteBarrierRun(jobID)
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

// checkpointAll runs a barrier for every job holding an open file.
//
// The set comes from the assembler rather than from the queue, because "has an
// open file" is the assembler's fact and R8 bounds barrier cost by exactly
// that set: a barrier fsyncs open files, not every file a job will eventually
// produce. Deriving the set from job status instead would be a second
// representation of the same fact, free to drift (S5).
func (app *Application) checkpointAll(ctx context.Context, perJob time.Duration) {
	if app.barrier == nil {
		return
	}
	jobs, err := app.assembler.OpenJobIDs(ctx)
	if err != nil {
		app.log.Warn("checkpoint skipped, could not list jobs with open files", "err", err)
		return
	}
	for _, jobID := range jobs {
		// Each job gets its own deadline, and that is what makes B4/R22 true
		// on this path rather than merely stated.
		//
		// SyncTarget.Drain, Sync and Truncate carry no timeout of their own:
		// the contract has them "bounded by the caller's deadline", and the
		// periodic caller had none — runCheckpoint is launched with the
		// application's lifetime context, cancelled only at shutdown. So a
		// worker parked in an fsync on a wedged mount blocked the barrier for
		// as long as the mount stayed down, and with it the single loop that
		// also owns stall re-evaluation and the queue save.
		//
		// PER JOB rather than per sweep, because a bound on the whole sweep
		// would let one wedged job consume the budget of every job behind it —
		// turning one bad mount into a queue-wide outage by a different route.
		jobCtx, cancel := context.WithTimeout(ctx, perJob)
		app.checkpointJob(jobCtx, jobID)
		cancel()
	}
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
		// ErrNotFound or ErrJobNotResident to both. Nothing downstream acts on
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
	mu.Lock()
	// --- Barrier serialised per job below this line ---
	err = app.barrier.FinalizeFile(ctx, jobID, int32(fileIdx), trunc) //nolint:gosec // G115: file counts are far below int32
	mu.Unlock()
	// --- No lock held below this line ---

	if err != nil {
		// Reported by the caller, not here: the lock spans the barrier's I/O
		// by design, and a log write is I/O of its own with no business
		// extending that span.
		return fmt.Errorf("%w: job %s file %d: %w", ErrNotFinalized, jobID, fileIdx, err)
	}
	app.recordAssembledCRC(ctx, jobID, fileIdx)
	return nil
}

// recordAssembledCRC copies the finalized file's whole-file CRC onto the queue,
// where QuickCheck and on-demand par2 read it.
//
// Both compare a par2 set's recorded CRC32 against the one this download
// produced. With none recorded they read NoCRC and take the conservative
// branch: par2NeedsRecovery fetches every recovery volume even for a
// bit-perfect download, and QuickCheck can never bypass the full par2 verify.
// That is safe but it is not free, and it was the state of every download.
//
// The value comes from Class A, combined over the facts that tile the file.
// That is the source #349's combine lacked: the assembler could only see the
// articles THIS run fetched, so a resumed file's parts did not tile and the
// figure described a fragment. Facts persist, so they name every article of
// the file whichever run fetched it.
//
// Best effort by design. A missing CRC costs a full verify, which is exactly
// today's behaviour, so a failure here must not fail the finalize that has
// already committed the extent and acked the articles.
func (app *Application) recordAssembledCRC(ctx context.Context, jobID string, fileIdx int) {
	if app.extents == nil {
		return
	}
	exts, err := app.extents.Load(ctx, jobID)
	if err != nil {
		app.log.Debug("load extents to record the assembled CRC", "job", jobID, "err", err)
		return
	}
	for _, e := range exts {
		if int(e.FileIdx) != fileIdx {
			continue
		}
		// HasPrefixCRC is the R23 distinction between "unavailable" and "zero".
		// A file with a permanently failed article has a prefix that stops at
		// the hole, and recording that as the file's CRC would report a
		// mismatch against par2 for a file that is merely incomplete.
		if !e.HasPrefixCRC {
			return
		}
		if err := app.queue.SetFileCRC32(jobID, fileIdx, e.PrefixCRC); err != nil {
			app.log.Debug("record the assembled CRC", "job", jobID, "fileidx", fileIdx, "err", err)
		}
		return
	}
}

// runCheckpoint is R6's cadence: a barrier per job on the lesser of a time
// bound and a byte bound, and a queue save after it.
//
// The queue save follows the barrier rather than running on its own timer
// because the barrier is what produces something worth saving. An ack marks
// articles Done in memory; until the queue is written, a crash re-fetches
// them anyway, so saving first would persist a snapshot that is stale by
// construction.
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
	if !app.queue.IsDirty() {
		return
	}
	adminDir := app.config.GetGeneral().AdminDir
	if err := app.queue.Save(filepath.Join(adminDir, "queue")); err != nil {
		app.log.Warn("periodic queue save failed", "err", err)
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
	if app.barrier == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownCheckpointTimeout)
	defer cancel()
	app.checkpointAll(ctx, shutdownCheckpointTimeout)
	app.saveQueueIfDirty()
}

// appendArticleFacts records Class A for one decoded article.
//
// Deliberately not ordered against the write. A Class A fact says only "if the
// bytes at [Offset, Offset+Length) are present, they hash to CRC32", which is
// true the instant the article is decoded and stays true whether the write
// succeeded, failed, or was never attempted. That independence is the whole
// reason Class A needs no barrier (R2), and adding an ordering here would
// quietly destroy it.
//
// # Why the caller's cancellation is dropped
//
// The append runs on a context.WithoutCancel copy of the caller's, bounded by
// its own timeout. Shutdown cancels the pipeline's context while an article is
// still being applied, and the write below it can still land — the assembler
// takes the buffer, the shutdown checkpoint fsyncs it and acks it. On the
// caller's context the fact insert loses that race and fails with "context
// canceled", so the committed extent claims an article durable that Class A
// cannot prove. The next resume reads the file, finds no recorded region for
// it, and re-fetches bytes that are sitting on disk.
//
// Measured before this change, on the crash harness's clean stop-restart: one
// article in roughly one run in ten, always the last one applied, which is the
// only one that can still be in flight when the cancel arrives. It is bounded
// rework rather than a correctness fault — that is R3, and the fallback below
// still holds — but it is rework a clean shutdown should not be paying at all,
// and it was invisible until the startup sweep became authoritative over
// job_files.articles_done (#362).
//
// Dropping the cancellation is safe in the direction that matters. A fact is
// unconditionally true when it is minted and asserts nothing about presence,
// so recording one can never over-claim; the worst case is a fact for bytes
// that never reached disk, which a resume then fails to verify and leaves
// Outstanding. The timeout is what keeps this bounded: it must not turn a
// shutdown into an unbounded wait on a contended database.
//
// # Why the apply loop is NOT also stopped, which Task 12b left open
//
// The paragraph above describes a race, and it is worth being exact about it
// rather than reading "the checkpoint acks it" as a guarantee. On a signal,
// signal.NotifyContext cancels the root context BEFORE Application.Shutdown
// runs, so pipeline.run returns at once, and its deferred close(work) +
// wg.Wait() lets the workers finish the results already buffered in `work` —
// up to 2*numWorkers of them — each with an already-cancelled ctx. Those
// applies run CONCURRENTLY with the shutdown checkpoint.
//
// Task 12b bounded Class A here and explicitly did not decide whether the
// apply loop should stop instead. It should not, for three reasons:
//
//   - Stopping it moves the wrong way. Those articles are already downloaded
//     and decoded; dropping them guarantees a re-fetch, where letting them
//     apply usually converts them into bytes on disk. The proposal costs
//     strictly more rework than the status quo.
//   - It cannot over-claim. The ack set is exactly what SyncTarget.Drain
//     returned, and Drain is a syncOp on the assembler's single worker
//     goroutine — the same goroutine and the same channel that serve
//     WriteArticle (X1). A late write therefore lands wholly inside the
//     drained set or wholly outside it; there is no interleaving that could
//     ack an article whose bytes are not covered by the fsync.
//   - Losing the ack is not losing the bytes. An article written after the
//     checkpoint's Drain is on disk with a Class A fact and no durable bit.
//     The write moved the file's mtime, so the next start fails Resume's
//     stat fast path, recomputes from the bytes, verifies the region against
//     its recorded CRC, and sets the bit. The article is recovered, not
//     re-fetched.
//
// So all three of "the write lands", "the fact records it" and "the
// checkpoint acks it" are wanted, but only the first two are ordered. The
// third is a race whose loss costs one recomputation on the next start,
// which is R3's bounded rework. This is correct by design, not a defect.
//
// A failure is bounded, but "a re-fetch and nothing else" is NOT the bound,
// and this comment claimed it was. The missing fact also makes the article
// invisible to every walk over Class A — including the two that derive the
// truncate bound in Barrier.FinalizeFile. The article is still drained,
// fsynced and given a truthful durable bit, so if it holds a completing
// file's top offset the fact-derived bound sits below its bytes and the
// truncate destroys them, while the same call acks it Done.
//
// FinalizeFile now counts durable articles the fact log does not name and
// declines to truncate at all when there are any, so the real cost of a
// failure here is: pre-allocation's trailing zeros survive on that file, and
// the article is re-fetched only if a later resume has to recompute (a
// mismatched size/mtime stamp), where unprovable resolves to Outstanding
// under S3.
func (p *pipeline) appendArticleFacts(ctx context.Context, jobID string, f durability.ArticleFact) {
	if p.factLog == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), factAppendTimeout)
	defer cancel()
	if err := p.factLog.Append(ctx, jobID, []durability.ArticleFact{f}); err != nil {
		p.log.Warn("append article fact; the article will be re-fetched after a restart",
			"job", jobID, "artidx", f.ArtIdx, "err", err)
	}
}

// deleteJobDurability drops a departed job's Class A and Class B rows.
//
// Both are keyed by job ID with no foreign key to the queue, so nothing else
// removes them: a job that finished or was deleted would leave its facts and
// extents behind forever.
func (app *Application) deleteJobDurability(ctx context.Context, jobID string) {
	if app.factLog != nil {
		if err := app.factLog.DeleteJob(ctx, jobID); err != nil {
			app.log.Warn("delete article facts for a departed job", "job", jobID, "err", err)
		}
	}
	if app.extents != nil {
		if err := app.extents.DeleteJob(ctx, jobID); err != nil {
			app.log.Warn("delete file extents for a departed job", "job", jobID, "err", err)
		}
	}
}

var _ durability.Stallable = (*Application)(nil)

// checkpointSettings resolves the two bounds from config, substituting the
// defaults for unset or nonsensical values.
//
// Neither can be disabled, and it is worth stating the reason accurately
// because the obvious version of it is wrong.
//
// A barrier is the only thing that acks a downloaded article WHILE THE JOB IS
// RUNNING, so with checkpoints off a job makes no visible progress and holds
// every article Outstanding until it stops. It does NOT follow that the work
// is re-fetched: Resume falls through to recompute when no committed extent
// exists, and recompute re-derives the done-set from Class A facts and the
// bytes on disk. What is actually lost is (a) the whole point of Class B,
// which exists so a restart does not have to re-read every partial file, and
// (b) anything the write cache had not flushed when the process died, which
// has no fact-plus-bytes pair to recover from.
//
// So "off" is not a faster mode, it is a mode that trades a bounded fsync
// cadence for an unbounded startup read plus real re-fetch.
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
// truncate, a bookkeeping error out of buildExtent — never reached routeFault
// and would otherwise halt the job with no reason attached at all.
func (app *Application) routeFinalizeFailure(jobID string, fileIdx int, path string, err error) {
	app.log.Error("completed file was not finalized; the job is halted rather than "+
		"shipping a file whose bytes are not known to be correct",
		"job", jobID, "fileidx", fileIdx, "err", err)

	if routed, ok := errors.AsType[*storagefault.Fault](err); ok {
		app.log.Debug("the fault was already routed by the barrier; not routing it again",
			"job", jobID, "permanent", routed.Permanent)
		if !routed.Permanent {
			app.notePendingFinalize(jobID, fileIdx)
		}
		return
	}
	//
	// A non-resident job is a queue-residency condition. retryFinalize already
	// treats this as landed and says why — the extent is committed, so the
	// bits are replayed from Class B once the job resumes — and the first
	// attempt has no reason to answer differently.
	if errors.Is(err, queue.ErrJobNotResident) {
		app.log.Debug("finalize committed its extent but could not ack a non-resident job; "+
			"the bits are replayed from Class B after the resume",
			"job", jobID, "fileidx", fileIdx)
		app.notePendingFinalize(jobID, fileIdx)
		return
	}
	f := storagefault.Classify("finalize", path, err)
	app.Stall(jobID, f)
	if !f.Permanent {
		app.notePendingFinalize(jobID, fileIdx)
	}
}
