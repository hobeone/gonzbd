package app

import (
	"context"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestStall_DoesNotParkAJobWhileTheProcessIsStopping pins the one pause that
// cannot be undone.
//
// A pause taken during shutdown is PERSISTED with the job's queue row, and the
// stall list that would re-evaluate it is in-memory and dies with the process,
// so the next start restores the job paused and only the user resumes it. A
// healthy job came back Paused after a slow but normal stop, permanently.
//
// The discriminator is whether the PROCESS is stopping, not the error — see
// the sibling test for why the error cannot serve. This test drives the
// context half; the stopping flag is the other.
func TestStall_DoesNotParkAJobWhileTheProcessIsStopping(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)

	ctx, cancel := context.WithCancel(context.Background())
	application.ctx = ctx
	cancel() // the process is stopping

	application.Stall(j.ID(), storagefault.Classify("sync", "/downloads/a.bin", context.DeadlineExceeded))

	row, ok := application.dispatcher.Row(j.ID())
	if !ok {
		t.Fatal("row is not found")
	}
	if row.Status() == constants.StatusPaused {
		t.Errorf("the job was paused because the process was stopping; that pause is "+
			"persisted by the final queue save and nothing ever undoes it: stall reason=%q",
			application.StallReason(j.ID()).Reason)
	}
}

// TestStall_StillParksOnTheSameErrorWhenNotStopping is the half that keeps the
// guard above honest, and it is why the guard tests the context rather than
// the error.
//
// A wedged mount produces the identical context.DeadlineExceeded without any
// shutdown. That one MUST park the job
// with a reason: a job left running against a dead mount sits at 99% with
// nothing surfaced, which is exactly the silence A2 forbids. The two cases are
// indistinguishable by error value, so a guard keyed on the error would have
// swallowed both.
func TestStall_StillParksOnTheSameErrorWhenNotStopping(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)

	// Grounding: the application must NOT be stopping, or this test passes for
	// the same reason the one above does.
	if application.ctx != nil && application.ctx.Err() != nil {
		t.Fatal("the fixture is already stopping, so it cannot observe the running case")
	}

	application.Stall(j.ID(), storagefault.Classify("sync", "/downloads/a.bin", context.DeadlineExceeded))

	row, ok := application.dispatcher.Row(j.ID())
	if !ok {
		t.Fatal("row is not found")
	}
	if row.Status() != constants.StatusPaused {
		t.Fatal("a deadline against a wedged mount left the job running; it " +
			"sits at N% with no reason the operator can act on")
	}
	if application.StallReason(j.ID()).Reason == "" {
		t.Error("the job was parked with no reason attached (R27)")
	}
}
