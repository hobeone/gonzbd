package job

import (
	"errors"
	"testing"
	"time"
)

func testClock() time.Time { return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC) }

func TestNewAttempt_StartsFetching(t *testing.T) {
	a := newAttempt(testClock())
	v := a.view()
	if v.State != Fetching {
		t.Errorf("State = %v, want Fetching; an attempt opens when a lease is issued", v.State)
	}
	if v.Outcome != OutcomePending {
		t.Errorf("Outcome = %v, want OutcomePending", v.Outcome)
	}
	if !a.isOpen() {
		t.Error("isOpen() = false, want true for a fresh attempt")
	}
	if !a.started.Equal(testClock()) {
		t.Errorf("started = %v, want %v", a.started, testClock())
	}
}

func TestAttempt_TransitionRejectsIllegalEdge(t *testing.T) {
	a := newAttempt(testClock())
	// Fetching -> Extracting skips assessment.
	err := a.transition(Extracting, testClock())
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("transition(Extracting) error = %v, want ErrIllegalTransition", err)
	}
	if got := a.view().State; got != Fetching {
		t.Errorf("State = %v after a rejected transition, want Fetching unchanged", got)
	}
}

// TestAttempt_AssessedLatches pins the flag ToSABnzbd uses to tell a
// first-pass download from a re-entry fetching recovery volumes.
func TestAttempt_AssessedLatches(t *testing.T) {
	a := newAttempt(testClock())
	if a.view().Assessed {
		t.Fatal("Assessed = true on a fresh attempt, want false")
	}
	mustTransition(t, &a, Assessing)
	if !a.view().Assessed {
		t.Error("Assessed = false after entering Assessing, want true")
	}
	mustTransition(t, &a, Fetching)
	if !a.view().Assessed {
		t.Error("Assessed = false after leaving Assessing, want true; the flag latches for the attempt")
	}
}

// TestAttempt_ActivityClearsOnTransition pins that Activity never survives a
// state change. A stale activity would render as "repairing" while the job is
// extracting, which is worse than showing nothing.
func TestAttempt_ActivityClearsOnTransition(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	a.setActivity(ActPar2Verify)
	if got := a.view().Activity; got != ActPar2Verify {
		t.Fatalf("Activity = %v, want ActPar2Verify", got)
	}
	mustTransition(t, &a, Extracting)
	if got := a.view().Activity; got != ActNone {
		t.Errorf("Activity = %v after a transition, want ActNone", got)
	}
}

func TestAttempt_HoldRecordsNextAndReason(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.hold(Assessing, NoComputeSlot); err != nil {
		t.Fatalf("hold: %v", err)
	}
	v := a.view()
	if v.State != Waiting || v.Next != Assessing || v.Reason != NoComputeSlot {
		t.Errorf("view = %+v; want State=Waiting Next=Assessing Reason=NoComputeSlot", v)
	}
}

func TestAttempt_HoldRejectsAnIllegalDestination(t *testing.T) {
	a := newAttempt(testClock())
	// Fetching cannot resume into Repairing, so it must not be able to wait
	// for it either — otherwise the hold defers an illegal edge instead of
	// rejecting it.
	if err := a.hold(Repairing, NoComputeSlot); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("hold(Repairing) error = %v, want ErrIllegalTransition", err)
	}
	if got := a.view().State; got != Fetching {
		t.Errorf("State = %v after a rejected hold, want Fetching unchanged", got)
	}
}

func TestAttempt_FinishIsWriteOnce(t *testing.T) {
	a := newAttempt(testClock())
	later := testClock().Add(time.Minute)
	if err := a.finish(OutcomeCancelled, later); err != nil {
		t.Fatalf("finish: %v", err)
	}
	v := a.view()
	if v.State != Finished || v.Outcome != OutcomeCancelled {
		t.Fatalf("view = %+v; want State=Finished Outcome=Cancelled", v)
	}
	if a.isOpen() {
		t.Error("isOpen() = true after finish, want false")
	}
	if !a.ended.Equal(later) {
		t.Errorf("ended = %v, want %v", a.ended, later)
	}

	err := a.finish(OutcomeOK, later.Add(time.Minute))
	if !errors.Is(err, ErrOutcomeAlreadySet) {
		t.Fatalf("second finish error = %v, want ErrOutcomeAlreadySet", err)
	}
	if got := a.view().Outcome; got != OutcomeCancelled {
		t.Errorf("Outcome = %v after a rejected second finish, want Cancelled unchanged", got)
	}
}

func TestAttempt_FinishRejectsPending(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.finish(OutcomePending, testClock()); err == nil {
		t.Error("finish(OutcomePending) = nil, want an error; Pending is not a verdict")
	}
	if a.view().State == Finished {
		t.Error("attempt reached Finished with a Pending outcome")
	}
}

// TestAttempt_FinishRejectsUnrecognizedOutcome pins the resolution to a
// question deferred from Task 5: Outcome.IsSettled() reports true for any
// value other than OutcomePending, including one no const declares
// (Outcome(42)), because it is a zero-value check rather than a range check.
// finish must not persist a verdict the machine never produces.
func TestAttempt_FinishRejectsUnrecognizedOutcome(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.finish(Outcome(42), testClock()); err == nil {
		t.Error("finish(Outcome(42)) = nil, want an error; 42 is not a declared outcome")
	}
	if got := a.view(); got.State == Finished || got.Outcome != OutcomePending {
		t.Errorf("view = %+v after a rejected finish, want unchanged (Fetching, Pending)", got)
	}
}

// TestAttempt_FinishSucceedsFromAnyOpenState pins that finish reaches
// Finished from every non-terminal state reachable via legal transitions, not
// only from Fetching — needed because finish() has no CanTransition(state,
// Finished) guard of its own: CanTransition(s, Finished) is true for every s
// in AllStates() (legalEdges gives every non-terminal state a cancel edge),
// so such a guard could never reject anything and was removed rather than
// kept as an inert check.
func TestAttempt_FinishSucceedsFromAnyOpenState(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	mustTransition(t, &a, Extracting)
	if err := a.finish(OutcomeOK, testClock()); err != nil {
		t.Fatalf("finish from Extracting: %v", err)
	}
	if got := a.view().State; got != Finished {
		t.Errorf("State = %v, want Finished", got)
	}
}

func mustTransition(t *testing.T, a *Attempt, to State) {
	t.Helper()
	if err := a.transition(to, testClock()); err != nil {
		t.Fatalf("transition(%v): %v", to, err)
	}
}
