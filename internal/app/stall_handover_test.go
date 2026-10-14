package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestFinalizeHopeless_ClearsTheStallOfTheJobItHandsOver: a job Stall parked
// and a hopeless detector then hands to post-processing is on its way to
// history. Its stall record goes with the hand-over, not at the next
// re-evaluation, so StallReason reports nothing at once and no re-evaluation
// can resume a later attempt registered under the same ID.
func TestFinalizeHopeless_ClearsTheStallOfTheJobItHandsOver(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	application.Stall(j, testFault("write"))
	if application.StallReason(j.ID()).Reason == "" {
		t.Fatal("precondition: Stall recorded no reason")
	}

	application.finalizeHopeless(j.ID(), hopelessHealthGate)

	if !application.postProcAdmissions.has(j) {
		t.Fatal("precondition: the hopeless hand-over did not admit the job")
	}
	if got := application.StallReason(j.ID()).Reason; got != "" {
		t.Errorf("after the hopeless hand-over StallReason = %q, want none: the job has left the queue", got)
	}
}

// TestHandOverStall_AJobGoneSinceIsForgottenNotResumed: a deferred hand-over
// whose job left the dispatcher before the resume still clears the record
// Stall left, and the refused resume is not fatal.
func TestHandOverStall_AJobGoneSinceIsForgottenNotResumed(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	gone := job.New("feedface00944a01", "gone", job.Policy{})
	application.noteStallUnlessHandedOff(gone, testFault("write"), stallRetryable)

	application.handOverStall(gone, true)

	if got := application.StallReason(gone.ID()).Reason; got != "" {
		t.Errorf("after the hand-over StallReason = %q, want none", got)
	}
	if _, ok := application.dispatcher.Job(gone.ID()); ok {
		t.Error("the resume registered a job that had left the dispatcher")
	}
}

// TestFinalizeHopeless_OnAStalledJobAwaitingAssessing_ResumesItForAssessing:
// a hopeless hand-over deferred to the Assessing worker of a job Stall parked
// clears the record and lifts Stall's pause, as Fail's does, so the tick
// launches the worker the reason waits for.
func TestFinalizeHopeless_OnAStalledJobAwaitingAssessing_ResumesItForAssessing(t *testing.T) {
	t.Parallel()
	f := newFailAtAssessingFixture(t)
	f.stall(t)

	f.app.finalizeHopeless(f.j.ID(), hopelessHealthGate)

	if f.app.postProcAdmissions.has(f.j) {
		t.Fatal("finalizeHopeless admitted the job; its Assessing worker must hand it over")
	}
	if got := f.app.StallReason(f.j.ID()).Reason; got != "" {
		t.Errorf("after the deferred hopeless hand-over StallReason = %q, want none", got)
	}
	if got := f.j.Intent(); got != job.IntentRun {
		t.Errorf("after the deferred hopeless hand-over the job's intent is %v: Stall's pause was not lifted, so no Assessing worker launches", got)
	}
}
