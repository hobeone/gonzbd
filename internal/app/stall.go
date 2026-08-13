package app

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

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
	fault *storagefault.Fault
	since time.Time
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
	rec, ok := app.stalls[jobID]
	if !ok {
		rec = &stallRecord{files: map[int]finalizeState{}, since: time.Now()}
		app.stalls[jobID] = rec
	}
	rec.fault = f
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
	if !ok || rec.fault == nil {
		return StallInfo{}
	}
	return StallInfo{Reason: "Stalled: " + rec.fault.Error(), Since: rec.since}
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

// reevaluateStall tries to get one parked job moving again.
//
// The job is resumed FIRST, before anything is retried, and that ordering is
// not cosmetic. Application.Stall pauses the job, and Queue.Pause evicts its
// manifest; a retried finalize ends in Barrier.FinalizeFile's AckDurable,
// which needs the job resident and fails outright on one that is not. The
// resume is what re-promotes it. Retrying first looked safer — it kept an
// unfinalized file away from a running job — and it simply does not work: the
// finalize gets as far as committing the extent and then fails to ack, so the
// stall re-raises itself with a reason about residency rather than storage.
//
// Resuming before the file is finalized is nonetheless safe, and concern 8 is
// why: the assembler reports a file complete exactly once, so nothing in the
// resumed job's own path can mark this file complete. The only route to that
// is completeFinalizedFile below, which runs after the finalize has succeeded.
//
// Nothing here is idempotent by accident: each file is dropped from the
// recovery set only once the queue has accepted its completion, so a step that
// could not run this time runs at the next interval instead.
func (app *Application) reevaluateStall(ctx context.Context, jobID string) {
	files := app.recoveryFiles(jobID)
	for _, st := range files {
		if st == finalizeLost {
			// Nothing this process can do. The reason already says so.
			return
		}
	}

	if err := app.queue.Resume(jobID); err != nil {
		app.log.Warn("stall re-evaluation: the job could not be resumed", "job", jobID, "err", err)
		app.clearStall(jobID)
		return
	}
	app.log.Info("stall re-evaluated; the job has been resumed",
		"job", jobID, "files_to_recover", len(files))

	for _, fileIdx := range slices.Sorted(maps.Keys(files)) {
		if files[fileIdx] == finalizeDone {
			continue
		}
		if err := app.retryFinalize(ctx, jobID, fileIdx); err != nil {
			app.log.Info("stall re-evaluation: the file still cannot be finalized; the job stays parked",
				"job", jobID, "fileidx", fileIdx, "err", err)
			app.routeFinalizeFailure(jobID, fileIdx, app.filePathFor(jobID, fileIdx), err)
			return
		}
		app.setFinalizeState(jobID, fileIdx, finalizeDone)
	}

	// Deliver the completions the stall interrupted. Each needs the job
	// resident, which it is not if the active set was full when Resume ran —
	// so an entry survives to be tried again rather than being dropped.
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
// So the handle is checked first, and its absence is recorded as
// finalizeLost rather than as success. That state is terminal within this
// process: a restart re-derives the job's outstanding work from its committed
// extents, which is the path that does recover it.
func (app *Application) retryFinalize(ctx context.Context, jobID string, fileIdx int) error {
	if app.assembler == nil || app.barrier == nil {
		app.setFinalizeState(jobID, fileIdx, finalizeLost)
		return fmt.Errorf("%w: job %s file %d: no barrier in this process", ErrNotFinalized, jobID, fileIdx)
	}
	open, err := app.assembler.OpenFiles(ctx, jobID)
	if err != nil {
		return fmt.Errorf("%w: job %s file %d: cannot tell whether it is still open: %w",
			ErrNotFinalized, jobID, fileIdx, err)
	}
	if !slices.Contains(open, int32(fileIdx)) { //nolint:gosec // G115: file counts are far below int32
		app.setFinalizeState(jobID, fileIdx, finalizeLost)
		app.stallLost(jobID, fileIdx)
		return fmt.Errorf("%w: job %s file %d: its handle is gone", ErrNotFinalized, jobID, fileIdx)
	}
	return app.finalizeCompletedFile(ctx, jobID, fileIdx)
}

// stallLost re-surfaces a stall whose file can no longer be finalized in this
// process, with the one action left.
//
// A2: the condition has a subject (this file) and a disposition (the job stays
// parked until a restart). Leaving the previous reason in place would tell the
// user to fix a mount they have already fixed.
func (app *Application) stallLost(jobID string, fileIdx int) {
	path := app.filePathFor(jobID, fileIdx)
	f := &storagefault.Fault{
		Op:   "finalize",
		Path: path,
		Err: fmt.Errorf("the completed file's handle was released before it could be trimmed; "+
			"restart gonzbd to resume job %s from its committed extents", jobID),
		Permanent: false,
	}
	app.noteStall(jobID, f)
	if err := app.queue.SetWarning(jobID, "Stalled: "+f.Error()); err != nil {
		app.log.Warn("stall: could not surface the reason", "job", jobID, "err", err)
	}
}
