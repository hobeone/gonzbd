package app

import (
	"context"
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestStall_DoesNotParkAJobWhileTheProcessIsStopping pins the one pause that
// cannot be undone.
//
// Shutdown's final queue.Save PERSISTS a pause taken during shutdown, the
// stall list that would re-evaluate it is in-memory and dies with the process,
// and the startup sweep skips the job because its phase is no longer active.
// A healthy job came back Paused after a slow but normal stop, permanently.
//
// The trigger is ordinary: the clean-shutdown barrier runs under a fixed
// deadline, and on a queue with many open files the fsyncs exceed it, so every
// job it reaches raises a deadline-exceeded fault.
//
// The discriminator is whether the PROCESS is stopping, not the error — see
// the sibling test for why the error cannot serve. This test drives the
// context half; TestStall_TheStoppingGuardCoversTheCleanShutdownBarrier drives
// the flag, which is the half that covers Shutdown's own barrier.
func TestStall_DoesNotParkAJobWhileTheProcessIsStopping(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	ctx, cancel := context.WithCancel(context.Background())
	application.ctx = ctx
	cancel() // the process is stopping

	application.routeFinalizeFailure(job.ID, 0, "/downloads/a.bin", context.DeadlineExceeded)

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("snap is nil")
	}
	if snap.Status == constants.StatusPaused {
		t.Errorf("the job was paused because the process was stopping; that pause is "+
			"persisted by the final queue save and nothing ever undoes it: warning=%q",
			snap.Warning)
	}
}

// TestStall_StillParksOnTheSameErrorWhenNotStopping is the half that keeps the
// guard above honest, and it is why the guard tests the context rather than
// the error.
//
// A wedged mount produces the identical context.DeadlineExceeded, through
// barrierOpTimeout rather than through shutdown. That one MUST park the job
// with a reason: a job left running against a dead mount sits at 99% with
// nothing surfaced, which is exactly the silence A2 forbids. The two cases are
// indistinguishable by error value, so a guard keyed on the error would have
// swallowed both.
func TestStall_StillParksOnTheSameErrorWhenNotStopping(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	// Grounding: the application must NOT be stopping, or this test passes for
	// the same reason the one above does.
	if application.ctx != nil && application.ctx.Err() != nil {
		t.Fatal("the fixture is already stopping, so it cannot observe the running case")
	}

	application.routeFinalizeFailure(job.ID, 0, "/downloads/a.bin", context.DeadlineExceeded)

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("snap is nil")
	}
	if snap.Status != constants.StatusPaused {
		t.Fatal("a barrierOpTimeout against a wedged mount left the job running; it " +
			"sits at N% with no reason the operator can act on")
	}
	if snap.Warning == "" {
		t.Error("the job was parked with no reason attached (R27)")
	}
}

// TestRouteFinalizeFailure_DoesNotStallOnANonResidentJob pins the second
// non-storage error to reach this path.
//
// AckDurable answers queue.ErrJobNotResident when the job was evicted between
// the barrier's file listing and the ack. retryFinalize already treats that as
// landed and documents why — the runs are committed, so the bits are replayed
// from the record after the resume — but the FIRST-attempt path classified it as
// storage and stalled the job over a queue-residency condition.
func TestRouteFinalizeFailure_DoesNotStallOnANonResidentJob(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	application.routeFinalizeFailure(job.ID, 0, "/downloads/a.bin", queue.ErrJobNotResident)

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("snap is nil")
	}
	if snap.Status == constants.StatusPaused {
		t.Errorf("the job was stalled over a queue-residency condition, not a storage "+
			"one; retryFinalize treats the same error as landed: warning=%q", snap.Warning)
	}
}

// TestRouteFinalizeFailure_StillStallsOnARealStorageError is the grounding
// half. The two tests above must not pass because routing was disabled
// wholesale — an EIO on the finalize path still has to park the job with a
// reason an operator can act on.
func TestRouteFinalizeFailure_StillStallsOnARealStorageError(t *testing.T) {
	application, job := newDurabilityTestApp(t, 1, 2)

	application.routeFinalizeFailure(job.ID, 0, "/downloads/a.bin", errRealDiskFailure)

	snap := application.queue.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("snap is nil")
	}
	if snap.Status != constants.StatusPaused {
		t.Fatal("a real storage error did not stall the job; the file's bytes are not " +
			"known to be correct and it must not ship")
	}
	if snap.Warning == "" {
		t.Error("the job was stalled with no reason attached (R27)")
	}
}

// errRealDiskFailure stands in for an unrecognised device error, which R18
// makes retryable by default.
var errRealDiskFailure = errors.New("input/output error on the volume")
