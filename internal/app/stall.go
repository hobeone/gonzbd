package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// stallRecheckInterval is R19's "re-evaluated on an interval".
//
// Independent of the checkpoint interval, which measures rework and is
// therefore tuned for a healthy job. This one measures how long a user waits
// after clearing a full disk before their download moves again, and thirty
// seconds is short enough not to feel stuck while leaving a wedged mount alone
// most of the time — every re-evaluation of a still-broken condition costs a
// re-fault.
const stallRecheckInterval = 30 * time.Second

// finalizeState is where one completed file has got to in the recovery of a
// stall its finalize raised.
type finalizeState int

const (
	// finalizePending means the file's finalize has not yet succeeded: it must
	// be retried before the file may be marked complete.
	finalizePending finalizeState = iota
	// finalizeDone means the barrier trimmed the file and acked its last
	// drain, and only the queue side of the completion is left.
	finalizeDone
	// finalizeLost means the file's handle is gone, so no retry in this
	// process can finalize it. The job stays stalled with a reason that names
	// the one action left.
	finalizeLost
)

// stallRecord is one job's parked state: why it stopped, and what has to
// happen before it can move again.
type stallRecord struct {
	// reason is the rendered, surfaced text, not the fault it came from.
	//
	// A string rather than a *storagefault.Fault because not every reason IS
	// one. A completed file whose handle was released can never be finalized
	// in this process, and dressing that up as a "retryable storage fault"
	// told the operator to wait for a condition that will never clear —
	// with an empty path, since there is no longer a file to name. A2 asks
	// for an actionable reason, and the fault vocabulary cannot express this
	// one.
	reason string
	since  time.Time
	// files carries the completed files whose finalize the stall interrupted.
	//
	// This map is the whole of concern 8. When a file's parts have all
	// arrived, the assembler tombstones it and reports it complete exactly
	// once; if the finalize that follows fails, NOTHING re-triggers one. The
	// stall then survives the mount coming back, which is an L2 violation —
	// indefinite non-progress with a reason the user has already acted on.
	// Recording the file here is what gives the retry something to retry.
	files map[int]finalizeState
}

// stalledJobIDs returns the parked jobs in a stable order.
func (app *Application) stalledJobIDs() []string {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	return slices.Sorted(maps.Keys(app.stalls))
}

// noteStall records why a job is parked, without disturbing what an earlier
// stall already queued for recovery.
//
// A second fault on an already-stalled job replaces the reason — it is the
// more recent thing the user has to act on — but must not drop the interrupted
// finalizes, which are the only record that those files exist at all.
func (app *Application) noteStall(jobID string, f *storagefault.Fault) {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	app.setStallReasonLocked(jobID, "Stalled: "+f.Error())
}

// noteStallReason parks a job with a reason that is not a storage fault.
func (app *Application) noteStallReason(jobID, reason string) {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	app.setStallReasonLocked(jobID, reason)
}

// setStallReasonLocked records a reason, creating the record if needed.
func (app *Application) setStallReasonLocked(jobID, reason string) {
	rec, ok := app.stalls[jobID]
	if !ok {
		rec = &stallRecord{files: map[int]finalizeState{}, since: time.Now()}
		app.stalls[jobID] = rec
	}
	rec.reason = reason
}

// notePendingFinalize records a completed file whose finalize must be retried
// before the file may be marked complete.
func (app *Application) notePendingFinalize(jobID string, fileIdx int) {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	rec, ok := app.stalls[jobID]
	if !ok {
		rec = &stallRecord{files: map[int]finalizeState{}, since: time.Now()}
		app.stalls[jobID] = rec
	}
	if _, seen := rec.files[fileIdx]; !seen {
		rec.files[fileIdx] = finalizePending
	}
}

// setFinalizeState advances one file through the recovery states.
func (app *Application) setFinalizeState(jobID string, fileIdx int, st finalizeState) {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	if rec, ok := app.stalls[jobID]; ok {
		rec.files[fileIdx] = st
	}
}

// completeFinalizeRecovery drops one file from a job's recovery set, and the
// whole record once nothing is left to recover.
func (app *Application) completeFinalizeRecovery(jobID string, fileIdx int) {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	rec, ok := app.stalls[jobID]
	if !ok {
		return
	}
	delete(rec.files, fileIdx)
	if len(rec.files) == 0 {
		delete(app.stalls, jobID)
	}
}

// clearStall forgets a job's parked state.
func (app *Application) clearStall(jobID string) {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	delete(app.stalls, jobID)
}

// StallInfo is what a job's parked state looks like from outside the package.
type StallInfo struct {
	// Reason is the surfaced, actionable text R27 requires, or "" when the job
	// is not stalled.
	Reason string
	// Since is when the job first parked.
	Since time.Time
}

// StallReason reports why a job is parked, for the queue listing (R26, R27).
//
// Read from here rather than from Job.Warning because the two have different
// lifetimes: re-evaluation resumes the job to find out whether the condition
// cleared, and Queue.Resume wipes the warning as it goes. A user polling the
// queue during a re-evaluation would see the reason blink out and come back.
func (app *Application) StallReason(jobID string) StallInfo {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	rec, ok := app.stalls[jobID]
	if !ok || rec.reason == "" {
		return StallInfo{}
	}
	return StallInfo{Reason: rec.reason, Since: rec.since}
}

// ReevaluateStalls asks the checkpoint loop to re-evaluate every parked job
// now, rather than at the next interval. This is R19's "and on user action".
//
// Non-blocking: it is called from the API's resume handlers, and a
// re-evaluation does barrier I/O against the very mount that is suspected of
// being wedged. An HTTP handler must not wait for that.
func (app *Application) ReevaluateStalls() {
	select {
	case app.stallKick <- struct{}{}:
	default:
		// A re-evaluation is already queued; a second one would do the same
		// work. Dropping it is not a lost request.
	}
}

// reevaluateStalls re-evaluates every parked job (R19).
func (app *Application) reevaluateStalls(ctx context.Context) {
	for _, jobID := range app.stalledJobIDs() {
		if ctx.Err() != nil {
			return
		}
		app.reevaluateStall(ctx, jobID)
	}
}

// errFinalizeUnrecoverable reports a completed file no retry in this process
// can finalize, because its handle is gone.
//
// A sentinel rather than a storage fault, because it is neither: the disk is
// fine and there is nothing to wait for. Routing it through
// storagefault.Classify produced "Stalled: storage retryable fault on
// finalize \"\"" — an empty path, and the one instruction that would actually
// help erased. A1 read backwards.
var errFinalizeUnrecoverable = errors.New("app: the completed file's handle is gone")

// reevaluateStall tries to get one parked job moving again.
//
// # The automatic cadence does not resume a job until its finalizes have landed
//
// The first draft resumed first, because Barrier.FinalizeFile ends in
// AckDurable and that needs a resident job, which Stall -> Queue.Pause evicted.
// It works, and it costs too much: an unpaused job dispatches articles into the
// device that has just refused them, for the length of every retry, every
// interval, forever — contradicting the reason Stall pauses at all.
//
// The ack is the ONLY part that needs residency, and it is also the only part
// that is recoverable afterwards: ExtentStore.Commit runs before it, so a
// finalize that fails at the ack has already put the durable bits on stable
// record. Phase 3 replays them with SeedFromExtents, exactly as the startup
// sweep does. So a residency error is treated as the finalize having landed,
// and everything else keeps the job parked without it ever dispatching.
//
// That last claim is about THIS function, not about the whole system. A user
// Resume is outside it by design: the API's queue resume handlers unpause the
// job and then ask for a re-evaluation, so between those two the job is
// running whether or not the condition has cleared. A user who has fixed the
// mount is entitled to that; one who has not gets their job parked again by
// the re-evaluation that follows. What this function guarantees is that the
// AUTOMATIC cadence never unparks a job whose finalize is still failing.
//
// # Every file is retried, not just the first
//
// Returning at the first failure left the rest of a job's interrupted
// finalizes untried until the following interval, one per 30 seconds, each
// holding a file handle in the meantime. The first failure is routed — it is
// the reason the operator sees — and the rest are logged and retried in the
// same pass.
//
// Nothing here is idempotent by accident: each file is dropped from the
// recovery set only once the queue has accepted its completion, so a step that
// could not run this time runs at the next interval instead.
func (app *Application) reevaluateStall(ctx context.Context, jobID string) {
	// A job that has left the queue has nothing to recover and nothing to
	// resume. Checked before phase 1 rather than left to Queue.Resume in phase
	// 2, because phase 1 returns early while anything is still blocked — so a
	// departed job with a pending finalize would be retried on every interval
	// for the life of the process, and its routed fault re-logged each time.
	if app.queue.SnapshotJob(jobID) == nil {
		app.log.Info("stall re-evaluation: the job has left the queue; forgetting its parked state",
			"job", jobID)
		app.clearStall(jobID)
		return
	}

	files := app.recoveryFiles(jobID)

	// Phase 1 — retry, while the job is still paused.
	var blocked int
	for _, fileIdx := range slices.Sorted(maps.Keys(files)) {
		switch files[fileIdx] {
		case finalizeDone:
			continue
		case finalizeLost:
			// Nothing this process can do, and the reason already says so.
			blocked++
			continue
		case finalizePending:
		}
		err := app.retryFinalize(ctx, jobID, fileIdx)
		switch {
		case err == nil:
			app.setFinalizeState(jobID, fileIdx, finalizeDone)
			files[fileIdx] = finalizeDone
		case errors.Is(err, errFinalizeUnrecoverable):
			app.stallLost(jobID, fileIdx)
			files[fileIdx] = finalizeLost
			blocked++
		default:
			if blocked == 0 {
				// The first failure is the one the operator is shown. The
				// rest are the same condition seen again.
				app.routeFinalizeFailure(jobID, fileIdx, app.filePathFor(jobID, fileIdx), err)
			} else {
				app.log.Info("stall re-evaluation: another file still cannot be finalized",
					"job", jobID, "fileidx", fileIdx, "err", err)
			}
			blocked++
		}
	}
	if blocked > 0 {
		app.log.Info("stall re-evaluation: the job stays parked",
			"job", jobID, "files_blocked", blocked, "files_to_recover", len(files))
		return
	}

	// Phase 2 — resume, which is what re-promotes the job and makes it
	// resident again.
	if err := app.queue.Resume(jobID); err != nil {
		app.log.Warn("stall re-evaluation: the job could not be resumed", "job", jobID, "err", err)
		app.clearStall(jobID)
		return
	}
	app.log.Info("stall re-evaluated; the job has been resumed",
		"job", jobID, "files_recovered", len(files))

	// Phase 3 — replay what the retries committed but could not ack.
	if len(files) > 0 {
		app.seedFromCommittedExtents(ctx, jobID)
	}

	// Phase 4 — deliver the completions the stall interrupted. Each needs the
	// job resident, which it is not if the active set was full when Resume ran
	// — so an entry survives to be tried again rather than being dropped.
	for _, fileIdx := range slices.Sorted(maps.Keys(files)) {
		if err := app.completeFinalizedFile(ctx, FileComplete{JobID: jobID, FileIdx: fileIdx}); err != nil {
			app.log.Info("stall re-evaluation: the completion could not be delivered yet",
				"job", jobID, "fileidx", fileIdx, "err", err)
			continue
		}
		app.completeFinalizeRecovery(jobID, fileIdx)
	}
	if len(files) == 0 {
		app.clearStall(jobID)
	}
	app.emit(Event{Type: "queue_updated", NzoID: jobID})
}

// seedFromCommittedExtents installs a job's committed Class B bits into its
// live work set, so a retry that finalized a file while the job was paused
// does not leave that file's articles Outstanding.
//
// It is the same move the startup sweep makes, for the same reason: the extent
// is what a completed fsync stands behind, and SeedFromExtents is how that
// becomes the running job's belief about what is left to fetch. A failure here
// costs a re-fetch and nothing else, which is why it is logged rather than
// returned.
func (app *Application) seedFromCommittedExtents(ctx context.Context, jobID string) {
	if app.extents == nil {
		return
	}
	exts, err := app.extents.Load(ctx, jobID)
	if err != nil {
		app.log.Warn("stall re-evaluation: could not load committed extents; the job will re-fetch",
			"job", jobID, "err", err)
		return
	}
	if err := app.queue.SeedFromExtents(jobID, exts); err != nil {
		app.log.Warn("stall re-evaluation: could not seed the work set; the job will re-fetch",
			"job", jobID, "err", err)
	}
}

// recoveryFiles copies one job's recovery set so the walk above holds no lock
// across the barrier I/O a retry does.
func (app *Application) recoveryFiles(jobID string) map[int]finalizeState {
	app.stallMu.Lock()
	defer app.stallMu.Unlock()
	rec, ok := app.stalls[jobID]
	if !ok {
		return nil
	}
	return maps.Clone(rec.files)
}

// retryFinalize re-runs the finalize a stall interrupted, refusing to report
// success unless it actually ran.
//
// finalizeCompletedFile returns nil for two very different situations: it
// finalized the file, or there was legitimately nothing to finalize. On the
// first attempt both are fine — a file nothing holds open is one nothing
// downstream will act on either. On a RETRY the second is not: the file's
// parts have all arrived and its completion is queued behind this call, so
// reporting success would mark complete a file that was never trimmed, ship
// pre-allocation's trailing zeros to par2 and report a healthy download as
// damaged. That is exactly the failure the stall was raised to prevent.
//
// So the handle is checked first, and its absence comes back as
// errFinalizeUnrecoverable rather than as success. The caller surfaces that as
// its own reason; classifying it as a storage fault is what erased the one
// instruction that helps.
//
// A residency error is the one failure treated as success, and the ordering
// inside FinalizeFile is why: ExtentStore.Commit runs before AckDurable, so an
// ack that could not reach a non-resident job left the durable bits on stable
// record anyway. The caller replays them. The handle is released here rather
// than by finalizeCompletedFile's own defer, which sees only a non-nil error
// and keeps it for a retry that is no longer needed.
func (app *Application) retryFinalize(ctx context.Context, jobID string, fileIdx int) error {
	if app.assembler == nil || app.barrier == nil {
		return fmt.Errorf("%w: job %s file %d: no barrier in this process",
			errFinalizeUnrecoverable, jobID, fileIdx)
	}
	open, err := app.assembler.OpenFiles(ctx, jobID)
	if err != nil {
		return fmt.Errorf("%w: job %s file %d: cannot tell whether it is still open: %w",
			ErrNotFinalized, jobID, fileIdx, err)
	}
	if !slices.Contains(open, int32(fileIdx)) { //nolint:gosec // G115: file counts are far below int32
		return fmt.Errorf("%w: job %s file %d", errFinalizeUnrecoverable, jobID, fileIdx)
	}
	// The second success-lookalike, and the reason it is checked here rather
	// than trusted. finalizeCompletedFile answers nil for a nil sync target,
	// which is safe on a first attempt because MarkFileComplete then refuses
	// the completion too. On a retry the completion is queued behind this
	// call and delivered on a LATER cycle, by which time the job may be
	// resident again — so the file would be recorded finalizeDone, never
	// having been trimmed, and shipped with pre-allocation's zeros.
	//
	// Retryable rather than terminal: a manifest that cannot be read now may
	// be readable after the mount comes back, and a job that is merely
	// unpromoted becomes resident again on its own.
	if app.syncTargetFor(jobID) == nil {
		return fmt.Errorf("%w: job %s file %d: the job has no readable manifest, so no barrier "+
			"can be run over it", ErrNotFinalized, jobID, fileIdx)
	}
	err = app.finalizeCompletedFile(ctx, jobID, fileIdx)
	if errors.Is(err, queue.ErrJobNotResident) {
		app.log.Debug("retried finalize committed its extent but could not ack a non-resident job; "+
			"the bits are replayed from Class B after the resume", "job", jobID, "fileidx", fileIdx)
		if cerr := app.assembler.CloseFile(ctx, jobID, int32(fileIdx)); cerr != nil { //nolint:gosec // G115: file counts are far below int32
			app.log.Debug("close finalized file handle", "job", jobID, "fileidx", fileIdx, "err", cerr)
		}
		return nil
	}
	return err
}

// stallLost re-surfaces a stall whose file can no longer be finalized in this
// process, with the one action left.
//
// A2: the condition has a subject (this file) and a disposition (the job stays
// parked until a restart, which re-derives its outstanding work from committed
// extents). Leaving the storage reason in place would tell the operator to fix
// a mount they have already fixed; dressing this up as a retryable storage
// fault told them to wait for a condition that cannot clear.
func (app *Application) stallLost(jobID string, fileIdx int) {
	app.setFinalizeState(jobID, fileIdx, finalizeLost)
	reason := fmt.Sprintf(
		"Stalled: completed file %d could not be trimmed and its handle has been released; "+
			"restart gonzbd to resume this job from its committed extents", fileIdx)
	if path := app.filePathFor(jobID, fileIdx); path != "" {
		reason = fmt.Sprintf(
			"Stalled: completed file %q could not be trimmed and its handle has been released; "+
				"restart gonzbd to resume this job from its committed extents", path)
	}
	app.noteStallReason(jobID, reason)
	if err := app.queue.SetWarning(jobID, reason); err != nil {
		app.log.Warn("stall: could not surface the reason", "job", jobID, "err", err)
	}
}
