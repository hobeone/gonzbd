package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestReevaluateStall_ABlockedParkedJobStillDeliversItsFinalizedFile: another
// file's completion can block a job the stall paused. The stall's resume is
// then refused, and the re-evaluation must still release the park and deliver
// the file it holds the only record of: dropping it leaves the file finalized
// and never marked complete, and the job wedged until a restart.
func TestReevaluateStall_ABlockedParkedJobStillDeliversItsFinalizedFile(t *testing.T) {
	application, j := newDurabilityTestApp(t, 1, 2)
	id := j.ID()

	if err := application.dispatcher.PauseJob(id); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	application.stallMu.Lock()
	application.setStallReasonLocked(id, "Stalled: fixture", true)
	application.stallMu.Unlock()
	application.noteUndeliveredCompletion(id, 0)

	if moved, err := application.dispatcher.BlockUnwanted(j, true); err != nil || !moved {
		t.Fatalf("BlockUnwanted = (%v, %v), want it to move", moved, err)
	}

	application.reevaluateStall(t.Context(), id)

	if !j.Progress().FileComplete(0) {
		t.Error("the finalized file was never delivered: the re-evaluation dropped its " +
			"only recovery record when the resume was refused")
	}
	if application.weParked(id) {
		t.Error("the park was not released after the fault cleared")
	}
	if in := j.Intent(); in != job.IntentPause {
		t.Errorf("Intent = %v, want IntentPause: a blocked job is resumed only by the user", in)
	}
	if st, _ := application.dispatcher.UnwantedState(id); st != unwanted.StateBlocked {
		t.Errorf("UnwantedState = %d, want blocked", st)
	}
}
