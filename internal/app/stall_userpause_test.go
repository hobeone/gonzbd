package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestReevaluateStall_DoesNotUndoAUserPause pins whose pause a re-evaluation
// is allowed to lift.
//
// Phase 2 resumed unconditionally, and a stall record exists for reasons that
// involve no pause of ours at all. The reachable one is ordinary: a user pauses
// a job, Queue.Pause evicts it, the next checkpoint's AckDurable fails with
// ErrJobNotResident, and checkpointJob calls noteNeedsSeed — which creates a
// record. On the next tick nothing is blocked, so phase 2 flipped Paused →
// Queued and cleared the warning, with no log saying so.
//
// It also could not settle. Handles stay open through a pause —
// CloseJobHandles runs only from maybeFinalize — so the next checkpoint fails
// the same way and the record is recreated as fast as it is cleared.
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
	application.noteStallReason(job.ID(), "Stalled: storage retryable fault on write")
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

	// Both paths that DO pause set it.
	application.noteStallReason("ours", "Stalled: something")
	if !application.weParked("ours") {
		t.Error("a job this application parked was not recognised as ours, so a " +
			"re-evaluation would leave it parked after the condition cleared (L2, R19)")
	}
}
