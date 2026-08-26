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
	err := a.transition(Extracting)
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

// TestAttempt_TransitionRejectsFinished pins that finish is the only door
// into Finished. Before this guard existed, transition(Finished, now) set
// state to Finished without ever assigning an Outcome — confirmed
// empirically: state=Finished outcome=Pending isOpen=true. Rejecting the
// edge here makes "Finished implies a settled Outcome" true by construction:
// finish is the only mutator that can produce Finished, and it always
// assigns Outcome in the same call that assigns state.
func TestAttempt_TransitionRejectsFinished(t *testing.T) {
	a := newAttempt(testClock())
	err := a.transition(Finished)
	if !errors.Is(err, ErrFinishRequired) {
		t.Fatalf("transition(Finished) error = %v, want ErrFinishRequired", err)
	}
	if got := a.view().State; got != Fetching {
		t.Errorf("State = %v after a rejected transition, want Fetching unchanged", got)
	}
	if !a.isOpen() {
		t.Error("isOpen() = false after a rejected transition, want true; the attempt never finished")
	}
}

// TestAttempt_FinishedNeverOpen walks one sequence (Assessing, Extracting,
// Finalizing, finish) and pins that it leaves state == Finished with
// isOpen() reporting false — finish is the only mutator that can reach
// Finished, and it always assigns Outcome in the same call that assigns
// state, so the two can never come apart on this path. It does not claim
// this holds over every reachable sequence; see the door-ownership tests
// (TestAttempt_TransitionRejectsFinished and friends) for that broader
// property.
func TestAttempt_FinishedNeverOpen(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	mustTransition(t, &a, Extracting)
	mustTransition(t, &a, Finalizing)
	if err := a.finish(OutcomeOK, testClock()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	// Asserted unconditionally, not as the left half of a "state == Finished
	// && isOpen()" check: that guarded form would pass silently if finish
	// ever stopped setting Finished at all, since the right-hand isOpen()
	// check would never even run.
	if got := a.view().State; got != Finished {
		t.Fatalf("State = %v, want Finished", got)
	}
	if a.isOpen() {
		t.Error("isOpen() = true after finish reached Finished, want false")
	}
	if err := a.transition(Finished); !errors.Is(err, ErrFinishRequired) {
		t.Fatalf("transition(Finished) after finish, error = %v, want ErrFinishRequired", err)
	}
}

// TestAttempt_FinishReportsWriteOnceBeforeBoundary pins the check ordering in
// finish: an attempt that BOTH crossed the boundary AND is already settled
// must report the write-once sentinel, not ErrUnrecoverableAfterBoundary.
// Write-once is the more fundamental invariant — a second finish call is
// wrong regardless of which outcome it carries — while the boundary guard
// only ever has something to say about OutcomeUnrecoverable specifically.
// Before this ordering fix, this exact sequence returned
// "cannot record Unrecoverable for an attempt past the ... boundary: state
// is Finished", which misreports a second-finish bug as a boundary bug.
func TestAttempt_FinishReportsWriteOnceBeforeBoundary(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	mustTransition(t, &a, Extracting)
	mustTransition(t, &a, Finalizing)
	if err := a.finish(OutcomeOK, testClock()); err != nil {
		t.Fatalf("first finish: %v", err)
	}
	err := a.finish(OutcomeUnrecoverable, testClock())
	if !errors.Is(err, errOutcomeAlreadySet) {
		t.Fatalf("second finish (crossed + settled) error = %v, want errOutcomeAlreadySet", err)
	}
	if errors.Is(err, ErrUnrecoverableAfterBoundary) {
		t.Errorf("second finish error = %v, must not also report ErrUnrecoverableAfterBoundary; write-once is checked first", err)
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
	if !errors.Is(err, errOutcomeAlreadySet) {
		t.Fatalf("second finish error = %v, want errOutcomeAlreadySet", err)
	}
	if got := a.view().Outcome; got != OutcomeCancelled {
		t.Errorf("Outcome = %v after a rejected second finish, want Cancelled unchanged", got)
	}
}

func TestAttempt_FinishRejectsPending(t *testing.T) {
	a := newAttempt(testClock())
	err := a.finish(OutcomePending, testClock())
	if !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("finish(OutcomePending) error = %v, want ErrInvalidOutcome; Pending is not a verdict", err)
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
	err := a.finish(Outcome(42), testClock())
	if !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("finish(Outcome(42)) error = %v, want ErrInvalidOutcome; 42 is not a declared outcome", err)
	}
	if got := a.view(); got.State == Finished || got.Outcome != OutcomePending {
		t.Errorf("view = %+v after a rejected finish, want unchanged (Fetching, Pending)", got)
	}
}

// TestAttempt_FinishInvalidOutcomeMessagesAreDistinct pins that the two
// ErrInvalidOutcome cases stay tellable apart in the message, even though
// both wrap the same sentinel: Pending is a well-formed zero value that
// simply is not a verdict, while 42 is not a declared outcome at all, and a
// caller reading the error text needs to see which one happened.
func TestAttempt_FinishInvalidOutcomeMessagesAreDistinct(t *testing.T) {
	a := newAttempt(testClock())
	pendingErr := a.finish(OutcomePending, testClock())
	b := newAttempt(testClock())
	unrecognizedErr := b.finish(Outcome(42), testClock())
	if pendingErr.Error() == unrecognizedErr.Error() {
		t.Errorf("finish(Pending) and finish(42) produced the same message %q; want distinct text for each case", pendingErr.Error())
	}
}

// TestAttempt_FinishRejectsUnrecoverableInProduction pins the invariant
// OutcomeUnrecoverable's doc comment claims: the verdict means the job never
// crossed the Correctness/Production boundary, so finish must refuse it once
// the attempt's state is IsProduction. Probe (pre-fix): finish(Unrecoverable)
// from Finalizing returned a nil error.
func TestAttempt_FinishRejectsUnrecoverableInProduction(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	mustTransition(t, &a, Extracting)
	mustTransition(t, &a, Finalizing)
	err := a.finish(OutcomeUnrecoverable, testClock())
	if !errors.Is(err, ErrUnrecoverableAfterBoundary) {
		t.Fatalf("finish(Unrecoverable) from Finalizing error = %v, want ErrUnrecoverableAfterBoundary", err)
	}
	if got := a.view(); got.State == Finished || got.Outcome != OutcomePending {
		t.Errorf("view = %+v after a rejected finish, want unchanged (Finalizing, Pending)", got)
	}
}

// TestAttempt_FinishSucceedsFromAnyOpenState pins that finish reaches
// Finished from every non-terminal state reachable via legal transitions, not
// only from one — needed because finish() has no CanTransition(state,
// Finished) guard of its own (an earlier one was removed as dead: cancelling
// out of any open state is a door finish alone owns, entirely bypassing
// legalEdges). That bypass is even more pronounced now that Waiting is gone —
// legalEdges no longer has an edge into Finished from anywhere at all —
// `git grep -n 'Finished' internal/job/transition.go` shows it only as a map
// key with an empty successor list. This table IS the coverage that bypass
// relies on, so it walks all five non-terminal, non-sentinel states.
func TestAttempt_FinishSucceedsFromAnyOpenState(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, a *Attempt)
	}{
		{"Fetching", func(t *testing.T, a *Attempt) {}},
		{"Assessing", func(t *testing.T, a *Attempt) {
			mustTransition(t, a, Assessing)
		}},
		{"Repairing", func(t *testing.T, a *Attempt) {
			mustTransition(t, a, Assessing)
			mustTransition(t, a, Repairing)
		}},
		{"Extracting", func(t *testing.T, a *Attempt) {
			mustTransition(t, a, Assessing)
			mustTransition(t, a, Extracting)
		}},
		{"Finalizing", func(t *testing.T, a *Attempt) {
			mustTransition(t, a, Assessing)
			mustTransition(t, a, Extracting)
			mustTransition(t, a, Finalizing)
		}},
	}
	// AllStates() has one terminal member (Finished); every other member
	// must appear above, or this table stops being the coverage the removed
	// guard relies on without anyone noticing.
	if want := len(AllStates()) - 1; len(tests) != want {
		t.Fatalf("table has %d cases, want %d (len(AllStates()) - 1, excluding Finished)", len(tests), want)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAttempt(testClock())
			tt.setup(t, &a)
			if err := a.finish(OutcomeOK, testClock()); err != nil {
				t.Fatalf("finish from %s: %v", tt.name, err)
			}
			if got := a.view().State; got != Finished {
				t.Errorf("State = %v, want Finished", got)
			}
		})
	}
}

func mustTransition(t *testing.T, a *Attempt, to State) {
	t.Helper()
	if err := a.transition(to); err != nil {
		t.Fatalf("transition(%v): %v", to, err)
	}
}

// TestAttempt_SetNextRecordsTheDestination pins the marker's basic contract.
func TestAttempt_SetNextRecordsTheDestination(t *testing.T) {
	a := newAttempt(testClock())
	if a.next != StateUnset {
		t.Fatalf("a fresh attempt has next = %v, want StateUnset (its work has not ended)", a.next)
	}
	if err := a.setNext(Assessing); err != nil {
		t.Fatalf("setNext(Assessing) from Fetching: %v", err)
	}
	if a.next != Assessing {
		t.Errorf("next = %v, want Assessing", a.next)
	}
}

// TestAttempt_SetNextIsWriteOncePerVisit is defect 3's pin, carried into the
// door that replaced hold. Without it a verdict of Repairing could be
// overwritten with Extracting and the job would cross the boundary SKIPPING
// REPAIR. Re-declaring the same value is a no-op, so a caller retrying is not
// punished.
func TestAttempt_SetNextIsWriteOncePerVisit(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	if err := a.setNext(Repairing); err != nil {
		t.Fatalf("setNext(Repairing): %v", err)
	}
	if err := a.setNext(Extracting); !errors.Is(err, ErrNextAlreadySet) {
		t.Fatalf("setNext(Extracting) over a Repairing verdict, error = %v, want ErrNextAlreadySet", err)
	}
	if a.next != Repairing {
		t.Fatalf("next = %v after a refused setNext; the refusal must not have partially applied", a.next)
	}
	if err := a.setNext(Repairing); err != nil {
		t.Errorf("setNext(Repairing) twice = %v, want nil (idempotent re-assertion)", err)
	}
}

// TestAttempt_SetNextRejectsANonEdge pins that a destination the current state
// could not reach directly cannot be recorded either.
func TestAttempt_SetNextRejectsANonEdge(t *testing.T) {
	a := newAttempt(testClock()) // Fetching
	if err := a.setNext(Finalizing); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("setNext(Finalizing) from Fetching, error = %v, want ErrIllegalTransition", err)
	}
	if err := a.setNext(StateUnset); err == nil {
		t.Error("setNext(StateUnset) = nil; the sentinel is not a destination")
	}
}

// TestAttempt_TransitionClearsNext pins §3.3 rule 3: the move consumes the
// marker, so an attempt that never re-enters Assessing cannot carry a stale
// verdict for the rest of its life.
func TestAttempt_TransitionClearsNext(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.setNext(Assessing); err != nil {
		t.Fatalf("setNext: %v", err)
	}
	if err := a.transition(Assessing); err != nil {
		t.Fatalf("transition(Assessing): %v", err)
	}
	if a.next != StateUnset {
		t.Errorf("next = %v after the move was taken, want StateUnset", a.next)
	}
}

// TestAttempt_TransitionRequiresNextWhenSet pins the single-decider property.
// From Assessing, legalEdges permits Fetching, Repairing and Extracting; once a
// verdict is recorded, nothing else may choose. This is transition's to == next
// check, whose ONLY remaining purpose this is now that Waiting is gone.
func TestAttempt_TransitionRequiresNextWhenSet(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	if err := a.setNext(Repairing); err != nil {
		t.Fatalf("setNext(Repairing): %v", err)
	}
	if err := a.transition(Extracting); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("transition(Extracting) against a Repairing verdict, error = %v, want ErrIllegalTransition; "+
			"Assessing is the only decider and its verdict must not be bypassable", err)
	}
	if err := a.transition(Repairing); err != nil {
		t.Errorf("transition(Repairing) matching the verdict: %v", err)
	}
}

// TestAttempt_TransitionAcceptsAnyLegalEdgeWhenNextIsUnset pins the other half:
// with no verdict recorded, the edge map alone decides.
func TestAttempt_TransitionAcceptsAnyLegalEdgeWhenNextIsUnset(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	if a.next != StateUnset {
		t.Fatalf("next = %v, want StateUnset", a.next)
	}
	if err := a.transition(Repairing); err != nil {
		t.Errorf("transition(Repairing) with no verdict recorded: %v", err)
	}
}

// TestAttempt_FinishClearsNext is Ruling A's pin: TestAttempt_FinishClearsNextAndReason
// is deleted (Reason no longer exists on Attempt), but finish clearing a
// stale next on the settled attempt is a property that must not silently
// vanish along with it. Before this fix would-be-Ruling-A regression: a
// cancel taken with a recorded verdict (e.g. Assessing having set
// next=Repairing) could leave a Finished attempt reporting a destination it
// will never move to.
func TestAttempt_FinishClearsNext(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	if err := a.setNext(Repairing); err != nil {
		t.Fatalf("setNext(Repairing): %v", err)
	}
	if err := a.finish(OutcomeCancelled, testClock()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if a.next != StateUnset {
		t.Errorf("next = %v after finish on a settled attempt, want StateUnset", a.next)
	}
}
