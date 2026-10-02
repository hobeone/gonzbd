package app

import (
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestReevaluateStall_DoesNotUndoAUserPause pins whose pause a re-evaluation
// is allowed to lift.
//
// A stall record exists for reasons that involve no pause of ours at all:
// checkpointJob calls noteNeedsSeed when its AckDurable finds the job evicted,
// which creates a record, and the job can be one the user paused. Nothing is
// blocked, so a phase 2 that resumed unconditionally flipped Paused → Queued
// and cleared the warning, with no log saying so.
func TestReevaluateStall_DoesNotUndoAUserPause(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	if err := application.dispatcher.PauseJob(job.ID()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// What checkpointJob does when its ack finds the job non-resident. It is
	// not a pause and carries no reason.
	application.noteNeedsSeed(job.ID())

	row, ok := application.dispatcher.Row(job.ID())
	if !ok || row.Status() != constants.StatusPaused {
		t.Fatal("the fixture is not paused, so it cannot observe the pause being undone")
	}

	application.reevaluateStall(t.Context(), job.ID())

	row, ok = application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("the job left the queue")
	}
	if row.Status() != constants.StatusPaused {
		t.Errorf("status = %v — the user's pause was undone within one re-evaluation "+
			"interval, with no log saying so, by a record that was never a pause of ours",
			row.Status())
	}
}

// TestReevaluateStall_StillResumesWhatItParked is the half that keeps the
// guard above honest: without it, "never resume" satisfies the test and a job
// parked by a storage fault stays parked after the operator fixes the mount,
// which is the L2 violation R19 exists to prevent.
func TestReevaluateStall_StillResumesWhatItParked(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	// What Application.Stall does: record the reason, then pause.
	application.noteStall(job.ID(), storagefault.Classify("write", "/mnt/dl/a.bin", syscall.ENOSPC), true)
	if err := application.dispatcher.PauseJob(job.ID()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	row, ok := application.dispatcher.Row(job.ID())
	if !ok || row.Status() != constants.StatusPaused {
		t.Fatal("the fixture is not paused, so it cannot observe the resume")
	}

	application.reevaluateStall(t.Context(), job.ID())

	row, ok = application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("the job left the queue")
	}
	if row.Status() == constants.StatusPaused {
		t.Error("a job this application parked on a storage fault was left parked after " +
			"the condition cleared: indefinite non-progress with a reason the user has " +
			"already acted on (L2, R19)")
	}
}

// TestReevaluateStall_KeepsAUserPauseStallWasCalledOn pins the case where the
// job is already paused by the user when a storage fault reaches Stall, as
// happens when a checkpoint barrier faults on a paused-and-resident job.
//
// Stall's own PauseJob then leaves the intent unchanged, so the pause is the user's, and the
// re-evaluation that follows the fault clearing must clear the stall reason
// without resuming the job.
func TestReevaluateStall_KeepsAUserPauseStallWasCalledOn(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	if err := application.dispatcher.PauseJob(job.ID()); err != nil {
		t.Fatalf("the user's pause: %v", err)
	}
	application.Stall(job.ID(), storagefault.Classify("write", "/mnt/dl/a.bin", syscall.EIO))

	if application.StallReason(job.ID()).Reason == "" {
		t.Fatal("Stall recorded no reason, so the fixture cannot observe it being cleared")
	}
	if application.weParked(job.ID()) {
		t.Error("Stall recorded the user's pause as this application's")
	}

	application.reevaluateStall(t.Context(), job.ID())

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("the job left the queue")
	}
	if row.Status() != constants.StatusPaused {
		t.Errorf("status = %v, want Paused: the fault clearing undid the user's pause", row.Status())
	}
	if got := application.StallReason(job.ID()).Reason; got != "" {
		t.Errorf("stall reason %q survived a re-evaluation with nothing blocked", got)
	}
}

// TestStall_ASecondFaultKeepsTheParkItOwns pins that a re-stall of a job Stall
// itself parked stays ours, though by then the job's intent is Pause: the
// intent Stall reads is its own earlier pause, not the user's.
func TestStall_ASecondFaultKeepsTheParkItOwns(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)
	fault := storagefault.Classify("write", "/mnt/dl/a.bin", syscall.EIO)

	application.Stall(job.ID(), fault)
	application.Stall(job.ID(), fault)

	if !application.weParked(job.ID()) {
		t.Fatal("a second fault on a job Stall parked released this application's claim on the pause")
	}
	application.reevaluateStall(t.Context(), job.ID())
	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("the job left the queue")
	}
	if row.Status() == constants.StatusPaused {
		t.Error("a job only Stall paused was left paused after the fault cleared")
	}
}

// TestStall_OnAnUnparkedRecordOfAUserPausedJob pins that a record
// noteNeedsSeed created, on a job the user paused, stays unparked when Stall
// then fires.
func TestStall_OnAnUnparkedRecordOfAUserPausedJob(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)
	if err := application.dispatcher.PauseJob(job.ID()); err != nil {
		t.Fatalf("the user's pause: %v", err)
	}
	application.noteNeedsSeed(job.ID())

	application.Stall(job.ID(), storagefault.Classify("write", "/mnt/dl/a.bin", syscall.EIO))

	if application.weParked(job.ID()) {
		t.Error("Stall claimed the pause of a job the user had paused, on an existing record")
	}
}

// TestWeParked pins the predicate directly, because its two false cases have
// different causes and only one of them is exercised end to end.
func TestWeParked(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)

	if application.weParked("never-seen") {
		t.Error("a job with no stall record at all was reported as parked by us")
	}

	// noteNeedsSeed and notePendingFinalize both CREATE a record without
	// pausing anything. Reading either as our pause is what undid the user's.
	application.noteNeedsSeed("seed-only")
	if application.weParked("seed-only") {
		t.Error("a record created by a failed ack was reported as our pause; " +
			"re-evaluating it resumes a job we never paused")
	}
	application.notePendingFinalize("finalize-only", 0)
	if application.weParked("finalize-only") {
		t.Error("a record created by an interrupted finalize was reported as our pause")
	}

	// The path that pauses (Stall, via noteStall with claimPause) sets it.
	application.noteStall("ours", storagefault.Classify("write", "/mnt/dl/a.bin", syscall.ENOSPC), true)
	if !application.weParked("ours") {
		t.Error("a job this application parked was not recognised as ours, so a " +
			"re-evaluation would leave it parked after the condition cleared (L2, R19)")
	}
}
