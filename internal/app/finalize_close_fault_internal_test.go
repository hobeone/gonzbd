package app

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// The close a finalize defers is the file's FIRST flush on the paths that
// return before the barrier runs, and a redundant second one after a finalize
// that committed. These tests fail the close on each kind of path and pin what
// the fault is allowed to do.
//
// The fault is the assembler's own bound expiring: arm parks the worker, so
// CloseFile's opClose is never answered and comes back as a retryable
// *storagefault.Fault. That is what a wedged mount produces, and it is the only
// close-time fault reachable from this package without a dead device.

// closeFaultLogs swaps the application's logger for one that records Debug and
// above, so a test can see both what was said about the close and at what
// level.
func closeFaultLogs(application *Application) *lockedBuffer {
	logs := &lockedBuffer{}
	application.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logs
}

// requireStoppedByTheClose asserts the shape of a completion stopped by a
// failed first flush: ErrNotFinalized, carrying the close's own fault, and a
// Warn naming it.
func requireStoppedByTheClose(t *testing.T, err error, logs *lockedBuffer) {
	t.Helper()
	if err == nil {
		t.Fatal("finalizeCompletedFile returned nil although its close — the file's only " +
			"flush on this path — failed; handleFileComplete then marks the file complete " +
			"and hands it to DirectUnpack and post-processing")
	}
	if !errors.Is(err, ErrNotFinalized) {
		t.Errorf("err = %v, want it to wrap ErrNotFinalized so the caller stops the completion", err)
	}
	if _, ok := errors.AsType[*storagefault.Fault](err); !ok {
		t.Errorf("err = %v, want the close's own storage fault in the chain, so the caller "+
			"routes it with the op and path that failed (R27)", err)
	}
	if !strings.Contains(logs.String(), "level=WARN msg=\"completed file's only flush failed") {
		t.Errorf("no Warn for the failed first flush; logs:\n%s", logs.String())
	}
}

// TestFinalizeCompletedFile_WithoutABarrier_ACloseFaultStopsTheCompletion pins
// the degraded mode's path. With no barrier nothing drains, syncs or trims the
// file before the close, so the close IS the flush.
func TestFinalizeCompletedFile_WithoutABarrier_ACloseFaultStopsTheCompletion(t *testing.T) {
	t.Parallel()
	application, job, arm, _ := newArmableWedgedApp(t)
	application.barrier = nil
	logs := closeFaultLogs(application)
	arm()

	err := application.finalizeCompletedFile(t.Context(), job.ID(), 0)

	requireStoppedByTheClose(t, err, logs)
}

// TestFinalizeCompletedFile_WithNoSyncTarget_ACloseFaultStopsTheCompletion pins
// the nil-target path: a job whose manifest is not resident gets no barrier, so
// here too the close is the file's only flush.
func TestFinalizeCompletedFile_WithNoSyncTarget_ACloseFaultStopsTheCompletion(t *testing.T) {
	t.Parallel()
	application, job, arm, _ := newArmableWedgedApp(t)
	logs := closeFaultLogs(application)
	// Armed before the eviction: parking the worker opens file 1, which needs
	// the manifest resident.
	arm()
	job.Evict()
	if application.syncTargetFor(job.ID()) != nil {
		t.Fatal("the evicted job still has a sync target; this test is about the nil-target return")
	}

	err := application.finalizeCompletedFile(t.Context(), job.ID(), 0)

	requireStoppedByTheClose(t, err, logs)
}

// TestHandleFileComplete_AFailedFirstFlushIsNotShipped pins the caller's half:
// the stopped completion marks nothing complete, parks the job with the close's
// reason, and records the file so the stall re-evaluation reaches it.
func TestHandleFileComplete_AFailedFirstFlushIsNotShipped(t *testing.T) {
	t.Parallel()
	application, job, arm, _ := newArmableWedgedApp(t)
	application.barrier = nil
	arm()

	application.handleFileComplete(t.Context(), FileComplete{JobID: job.ID(), FileIdx: 0})

	if job.Progress().FileComplete(0) {
		t.Error("the file was marked complete although its only flush failed; DirectUnpack, " +
			"job finalization and post-processing all act on that bit")
	}
	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("job left the dispatcher")
	}
	if row.Status() != constants.StatusPaused {
		t.Errorf("status = %v, want Paused — a retryable close fault is a storage condition (A1)",
			row.Status())
	}
	if !strings.Contains(application.StallReason(job.ID()).Reason, "close") {
		t.Errorf("stall reason = %q, want the close's own fault (R27)",
			application.StallReason(job.ID()).Reason)
	}
	if !application.hasPendingFinalize(job.ID(), 0) {
		t.Error("the file was not recorded for the stall re-evaluation, so nothing ever " +
			"surfaces that its handle is gone and the job stays parked with no way forward")
	}
}

// ackThenWedge is the application's own Acker, followed by arm: the barrier has
// drained, synced, trimmed, committed and acked by the time the worker parks,
// so only the operations after the ack — Confirm and the deferred close — meet
// the wedge.
type ackThenWedge struct {
	app *Application
	arm func()
}

func (a ackThenWedge) AckDurable(p durability.DurableProof) error {
	err := a.app.AckDurable(p)
	a.arm()
	return err
}

// TestFinalizeCompletedFile_ACloseFaultAfterACommittedFinalizeIsTolerated pins
// the other side. After the barrier has committed and acked, the close is a
// redundant second flush; its fault is logged and the completion proceeds.
func TestFinalizeCompletedFile_ACloseFaultAfterACommittedFinalizeIsTolerated(t *testing.T) {
	t.Parallel()
	application, job, arm, _ := newArmableWedgedApp(t)
	application.barrier = durability.NewBarrier(realStore(t, application),
		ackThenWedge{app: application, arm: arm}, application, slog.New(slog.DiscardHandler))
	logs := closeFaultLogs(application)

	if err := application.finalizeCompletedFile(t.Context(), job.ID(), 0); err != nil {
		t.Fatalf("finalizeCompletedFile = %v, want nil — the barrier committed and acked "+
			"the file, so stopping on the redundant close would park a download whose "+
			"bytes are already on stable record", err)
	}
	if !job.Progress().ArticleDone(0) {
		t.Fatal("the barrier did not ack the article; the finalize did not run to completion " +
			"and this test is not about the post-hoc path")
	}
	// Grounding: without a failed close this test would pass whatever the
	// defer did with one.
	if !strings.Contains(logs.String(), "level=DEBUG msg=\"close completed file handle\"") {
		t.Fatalf("the close did not fail, so nothing was tolerated; logs:\n%s", logs.String())
	}
}

// TestRouteFinalizeFailure_FailsTheJobOnAPermanentFaultNothingRouted pins the
// R20 half of the unrouted arm. A close-time EROFS reaches here without the
// barrier's marker, and a stall for it resumed the job at the next
// re-evaluation — nothing was recorded for retry — leaving it running with a
// file that will never complete.
func TestRouteFinalizeFailure_FailsTheJobOnAPermanentFaultNothingRouted(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	const path = "/mnt/ro/A.bin"

	fault := storagefault.Classify("close", path, syscall.EROFS)
	if !fault.Permanent {
		t.Fatal("EROFS is not classified permanent; this test is about the permanent branch")
	}
	err := fmt.Errorf("%w: job %s file %d: %w", ErrNotFinalized, job.ID(), 0, fault)
	application.routeFinalizeFailure(job.ID(), 0, path, err)

	if reason := application.StallReason(job.ID()).Reason; reason != "" {
		t.Errorf("stall reason = %q — a permanent fault was stalled, so the next "+
			"re-evaluation resumes a job that cannot complete (R20)", reason)
	}
	if application.hasPendingFinalize(job.ID(), 0) {
		t.Error("a permanent fault was recorded for retry")
	}
	if row, ok := application.dispatcher.Row(job.ID()); ok &&
		!strings.Contains(row.Header.FailReason, "Failed:") {
		t.Errorf("fail reason = %q, want the permanent fault's reason", row.Header.FailReason)
	}
}
