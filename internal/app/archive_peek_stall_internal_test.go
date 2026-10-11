package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestReevaluateStall_ABlockedParkedJobStaysPausedForTheUser: another file's
// completion can block a job the stall paused. The stall's resume is then
// refused, and the re-evaluation must still forget the stall, leaving the job
// paused for the user, whose resume approves it.
func TestReevaluateStall_ABlockedParkedJobStaysPausedForTheUser(t *testing.T) {
	application, j := newDurabilityTestApp(t, 1, 2)
	id := j.ID()

	if err := application.dispatcher.PauseJob(id); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	application.stallMu.Lock()
	application.setStallReasonLocked(id, "Stalled: fixture", true)
	application.stallMu.Unlock()

	if moved, err := application.dispatcher.BlockUnwanted(j, true); err != nil || !moved {
		t.Fatalf("BlockUnwanted = (%v, %v), want it to move", moved, err)
	}

	application.reevaluateStall(id)

	if application.weParked(id) {
		t.Error("the park was not released after the fault cleared")
	}
	if r := application.StallReason(id).Reason; r != "" {
		t.Errorf("StallReason = %q after the re-evaluation, want none", r)
	}
	if in := j.Intent(); in != job.IntentPause {
		t.Errorf("Intent = %v, want IntentPause: a blocked job is resumed only by the user", in)
	}
	if st, _ := application.dispatcher.UnwantedState(id); st != unwanted.StateBlocked {
		t.Errorf("UnwantedState = %d, want blocked", st)
	}
}
