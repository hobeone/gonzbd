package job

import (
	"errors"
	"fmt"
	"strings"
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
	// Fetching -> Repairing is not an edge in legalEdges, and both states are
	// Correctness, so this exercises the plain illegal-edge rejection. The
	// boundary-specific case is Assessing -> Extracting — the ONLY
	// Correctness -> Production edge legalEdges has — and it is refused for a
	// different reason (ErrCrossRequired); see
	// TestAttempt_TransitionRefusesTheBoundaryEdge. Every other
	// Correctness -> Production PAIR, Fetching -> Extracting included, is not
	// an edge at all and lands here instead, which
	// TestAttempt_TransitionRejectsNonEdgesIntoProduction pins.
	err := a.transition(Repairing)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("transition(Repairing) error = %v, want ErrIllegalTransition", err)
	}
	if got := a.view().State; got != Fetching {
		t.Errorf("State = %v after a rejected transition, want Fetching unchanged", got)
	}
}

// TestAttempt_TransitionRefusesTheBoundaryEdge pins that transition refuses
// the one Correctness -> Production edge with ErrCrossRequired even when the
// edge itself is legal in legalEdges (Assessing -> Extracting) — the caller
// must use Cross instead, because only Cross also yields the lease.
func TestAttempt_TransitionRefusesTheBoundaryEdge(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	if err := a.transition(Extracting); !errors.Is(err, ErrCrossRequired) {
		t.Fatalf("transition(Extracting) from Assessing, error = %v, want ErrCrossRequired", err)
	}
	if got := a.view().State; got != Assessing {
		t.Errorf("State = %v after a refused transition, want Assessing unchanged", got)
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
	mustCross(t, &a, Extracting)
	if got := a.view().Activity; got != ActNone {
		t.Errorf("Activity = %v after a transition, want ActNone", got)
	}
}

// TestAttempt_SettlingKeepsThePosition walks one sequence (Assessing,
// Extracting, Finalizing, finish) and pins the two facts that replaced the
// Finished state.
//
// Settledness is an Outcome fact: isOpen() is outcome == OutcomePending, so
// assigning the verdict is what closes the attempt, and finish is the only
// mutator that assigns it — enforced by
// TestOutcomeWrites_MatchTheEnumerationStatedInProse, not by a guard on a
// state value. There is no longer a door into settledness for transition to be
// refused at, because there is no longer a State naming it: what used to be
// ErrFinishRequired is now a compile error.
//
// The position SURVIVES settling, and that is the point of the change. While
// finish overwrote a.state, the attempt forgot where it had got to, and a
// shadow latch had to remember for it. Asserting Finalizing here is asserting
// that the latch is not needed.
func TestAttempt_SettlingKeepsThePosition(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	mustCross(t, &a, Extracting)
	mustTransition(t, &a, Finalizing)
	if err := a.finish(OutcomeOK, testClock()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	// Each asserted unconditionally rather than as halves of one condition: a
	// guarded form would pass silently if the first half stopped holding,
	// because the second would never run.
	if got := a.view().State; got != Finalizing {
		t.Errorf("State = %v after finish, want Finalizing — settling records a verdict, "+
			"it does not move the attempt or erase where it got to", got)
	}
	if a.isOpen() {
		t.Error("isOpen() = true after finish, want false")
	}
	if !a.crossed() {
		t.Error("crossed() = false for a settled attempt that reached Finalizing; " +
			"the position is what answers this now, and it survived settling")
	}
	if got := a.view().Outcome; got != OutcomeOK {
		t.Errorf("Outcome = %v, want OutcomeOK", got)
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
// has crossed", which misreports a second-finish bug as a boundary bug.
func TestAttempt_FinishReportsWriteOnceBeforeBoundary(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	mustCross(t, &a, Extracting)
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
	if v.State != Fetching || v.Outcome != OutcomeCancelled {
		t.Fatalf("view = %+v; want State=Fetching (unchanged by settling) Outcome=Cancelled", v)
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
	if !a.isOpen() {
		t.Error("attempt settled with a Pending outcome")
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
	if got := a.view(); !a.isOpen() || got.Outcome != OutcomePending {
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
	mustCross(t, &a, Extracting)
	mustTransition(t, &a, Finalizing)
	err := a.finish(OutcomeUnrecoverable, testClock())
	if !errors.Is(err, ErrUnrecoverableAfterBoundary) {
		t.Fatalf("finish(Unrecoverable) from Finalizing error = %v, want ErrUnrecoverableAfterBoundary", err)
	}
	if got := a.view(); !a.isOpen() || got.Outcome != OutcomePending {
		t.Errorf("view = %+v after a rejected finish, want unchanged (Finalizing, Pending)", got)
	}
}

// TestAttempt_FinishSucceedsFromAnyOpenState pins that finish reaches
// settling from every state reachable via legal transitions, not
// only from one — needed because finish() has no CanTransition(state,
// settled) guard of its own (an earlier one was removed as dead: cancelling
// out of any open state is a door finish alone owns, entirely bypassing
// legalEdges). That bypass is even more pronounced now that Waiting is gone —
// legalEdges has no notion of settling at all — there is no State naming it,
// so `git grep -n 'Finishe[d]' internal/job/transition.go` returns nothing; it
// was once a map
// key with an empty successor list. This table IS the coverage that bypass
// relies on, so it walks all five non-terminal, non-sentinel states.
func TestAttempt_FinishSucceedsFromAnyOpenState(t *testing.T) {
	// The outcome varies by state and that is part of the property, not a
	// detail. finish admits OutcomeOK only at Finalizing — "produced its files"
	// is not true of an attempt that has not moved anything to the destination,
	// and §3.3 gives Extracting SetNext(Finalizing) as its only completion — so
	// every other case settles Failed. What this table pins is that NO open
	// state strands: every one of them can be settled with SOME legal verdict,
	// which is why finish consults neither legalEdges nor next. Narrowing WHICH
	// verdict a position admits does not narrow THAT it can settle, and this
	// table is what keeps those two apart.
	tests := []struct {
		name    string
		outcome Outcome
		setup   func(t *testing.T, a *Attempt)
	}{
		{"Fetching", OutcomeFailed, func(t *testing.T, a *Attempt) {}},
		{"Assessing", OutcomeFailed, func(t *testing.T, a *Attempt) {
			mustTransition(t, a, Assessing)
		}},
		{"Repairing", OutcomeFailed, func(t *testing.T, a *Attempt) {
			mustTransition(t, a, Assessing)
			mustTransition(t, a, Repairing)
		}},
		{"Extracting", OutcomeFailed, func(t *testing.T, a *Attempt) {
			mustTransition(t, a, Assessing)
			mustCross(t, a, Extracting)
		}},
		{"Finalizing", OutcomeOK, func(t *testing.T, a *Attempt) {
			mustTransition(t, a, Assessing)
			mustCross(t, a, Extracting)
			mustTransition(t, a, Finalizing)
		}},
	}
	// Every member of AllStates() must appear above, or this table stops
	// being the coverage the removed guard relies on without anyone noticing.
	// There is no terminal member to exclude now: settling is an Outcome fact,
	// so every State is a state an attempt can settle FROM.
	if want := len(AllStates()); len(tests) != want {
		t.Fatalf("table has %d cases, want %d (len(AllStates()))", len(tests), want)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAttempt(testClock())
			tt.setup(t, &a)
			if err := a.finish(tt.outcome, testClock()); err != nil {
				t.Fatalf("finish(%v) from %s: %v", tt.outcome, tt.name, err)
			}
			if a.isOpen() {
				t.Errorf("isOpen() = true after finish from %s, want false", tt.name)
			}
		})
	}
}

// mustCross records to as next and then crosses to it — cross requires a.next
// already name the destination it is given, so a bare a.cross(to) call
// without a prior setNext would fail these tests' own precondition, not the
// thing they mean to exercise.
func mustCross(t *testing.T, a *Attempt, to State) {
	t.Helper()
	if err := a.setNext(to); err != nil {
		t.Fatalf("setNext(%v): %v", to, err)
	}
	if err := a.cross(to); err != nil {
		t.Fatalf("cross(%v): %v", to, err)
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
//
// Uses Fetching as the alternative destination, not Extracting: a Repairing
// verdict tried against Extracting would now be refused by the boundary check
// (ErrCrossRequired) before transition's to == next check is even reached,
// since Assessing is Correctness and Extracting is Production — see
// TestJob_CrossEnforcesTheVerdict's "refuses a destination the verdict did
// not name" subtest for that case. Fetching keeps this test isolated to the
// single-decider property alone.
func TestAttempt_TransitionRequiresNextWhenSet(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	if err := a.setNext(Repairing); err != nil {
		t.Fatalf("setNext(Repairing): %v", err)
	}
	if err := a.transition(Fetching); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("transition(Fetching) against a Repairing verdict, error = %v, want ErrIllegalTransition; "+
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
	// Both work successors, not one. legalEdges permits Assessing → Fetching
	// and Assessing → Repairing without a verdict (Extracting is Cross's and
	// is refused here by design). Driving only Repairing would leave a
	// narrowing of transition to that single edge undetected, while the
	// test's name went on promising every legal edge.
	for _, to := range []State{Fetching, Repairing} {
		a := newAttempt(testClock())
		mustTransition(t, &a, Assessing)
		if err := a.transition(to); err != nil {
			t.Errorf("transition(%v) with no verdict recorded: %v", to, err)
		}
	}
	if err := a.transition(Extracting); !errors.Is(err, ErrCrossRequired) {
		t.Errorf("transition(Extracting) = %v, want ErrCrossRequired — the boundary edge is Cross's alone", err)
	}
}

// TestAttempt_FinishClearsNext is Ruling A's pin: TestAttempt_FinishClearsNextAndReason
// is deleted (Reason no longer exists on Attempt), but finish clearing a
// stale next on the settled attempt is a property that must not silently
// vanish along with it. Before this fix would-be-Ruling-A regression: a
// cancel taken with a recorded verdict (e.g. Assessing having set
// next=Repairing) could leave a settled attempt reporting a destination it
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

// TestJob_CrossYieldsTheLeaseAtomically pins §3.5: state, crossed, next and the
// lease all move in ONE call. As two calls this is defect 5's shape — forgetting
// the surrender leaks a pool-A slot permanently and silently.
func TestJob_CrossYieldsTheLeaseAtomically(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	l := NewLease(1)
	if err := j.Grant(l); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := j.Transition(Assessing); err != nil {
		t.Fatalf("Transition(Assessing): %v", err)
	}
	if err := j.SetNext(Extracting); err != nil {
		t.Fatalf("SetNext(Extracting): %v", err)
	}

	got, err := j.Cross(Extracting)
	if err != nil {
		t.Fatalf("Cross(Extracting): %v", err)
	}
	if got != l {
		t.Errorf("Cross yielded %p, want the held lease %p", got, l)
	}
	if j.HoldsLease() {
		t.Error("the job still holds a lease after crossing; Extracting holds only a compute slot")
	}
	v := j.State()
	if v.State != Extracting {
		t.Errorf("State = %v, want Extracting", v.State)
	}
	if v.Next != StateUnset {
		t.Errorf("Next = %v after the move was taken, want StateUnset", v.Next)
	}
}

// TestJob_CrossEnforcesTheVerdict pins that Cross is not a hole in the
// single-decider property transition's to == next check protects. Without it a
// caller could cross from anywhere, to anywhere in Production, ignoring the
// verdict Assessing recorded.
func TestJob_CrossEnforcesTheVerdict(t *testing.T) {
	t.Run("refuses a destination the verdict did not name", func(t *testing.T) {
		j := newTestJob(t)
		mustBegin(t, j)
		mustJobTransition(t, j, Assessing)
		if err := j.SetNext(Repairing); err != nil {
			t.Fatalf("SetNext(Repairing): %v", err)
		}
		if _, err := j.Cross(Extracting); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("Cross(Extracting) against a Repairing verdict, error = %v, want ErrIllegalTransition", err)
		}
	})
	t.Run("refuses from a state that is not Assessing", func(t *testing.T) {
		// Fetching cannot stand in for "not Assessing" here: setNext requires
		// CanTransition(a.state, n), and Fetching's only successor is
		// Assessing, so SetNext(Extracting) from Fetching would itself be
		// refused before Cross is even called — guard 3 (a.next != to) would
		// then fire on the empty next, which is exactly the false-pass this
		// subtest must not repeat (see the comment below). Drive the job to
		// Extracting instead and attempt the NEXT Production edge from
		// there: guard 1 (IsProduction(Finalizing)) passes, guard 3
		// (next == Finalizing) passes because SetNext recorded it, so guard 2
		// (a.state != Assessing) is what actually fires — the attempt is in
		// Extracting, not Assessing. This also pins something stronger than
		// the subtest's name alone: Cross cannot be used to walk deeper into
		// Production once it has already crossed once.
		j := newTestJob(t)
		mustBegin(t, j) // Fetching
		mustJobTransition(t, j, Assessing)
		if err := j.SetNext(Extracting); err != nil {
			t.Fatalf("SetNext(Extracting): %v", err)
		}
		if _, err := j.Cross(Extracting); err != nil {
			t.Fatalf("Cross(Extracting): %v", err)
		}
		if err := j.SetNext(Finalizing); err != nil {
			t.Fatalf("SetNext(Finalizing): %v", err)
		}
		if _, err := j.Cross(Finalizing); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("Cross(Finalizing) from Extracting, error = %v, want ErrIllegalTransition", err)
		}
	})
	t.Run("refuses a non-Production destination", func(t *testing.T) {
		// SetNext(Repairing) first, so guard 3 (a.next != to) passes and
		// guard 1 (!IsProduction(to)) is the one that actually fires. Without
		// it, a.next stays StateUnset and guard 3 fires first on the SAME
		// error (ErrIllegalTransition) the assertion accepts, so the subtest
		// would pass even with guard 1 deleted outright — which mutation
		// testing confirmed it did.
		j := newTestJob(t)
		mustBegin(t, j)
		mustJobTransition(t, j, Assessing)
		if err := j.SetNext(Repairing); err != nil {
			t.Fatalf("SetNext(Repairing): %v", err)
		}
		if _, err := j.Cross(Repairing); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("Cross(Repairing), error = %v, want ErrIllegalTransition; Cross owns the boundary edge only", err)
		}
	})
}

// TestJob_CrossYieldsNilWhenNoLeaseIsHeld pins the case §3.9 calls out: a job
// may legitimately reach the crossing holding nothing, having been paused at
// Assessing{next: Extracting} and resumed. Cross must report that rather than
// assert, so the Queue's sole reclaimer can no-op on nil.
func TestJob_CrossYieldsNilWhenNoLeaseIsHeld(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	mustJobTransition(t, j, Assessing)
	if err := j.SetNext(Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	got, err := j.Cross(Extracting)
	if err != nil {
		t.Fatalf("Cross with no lease held: %v", err)
	}
	if got != nil {
		t.Errorf("Cross yielded %p, want nil", got)
	}
	if j.State().State != Extracting {
		t.Error("the crossing did not happen; holding no lease must not block it")
	}
}

// TestJob_TransitionRefusesTheBoundaryEdge pins that Cross is the SOLE door.
func TestJob_TransitionRefusesTheBoundaryEdge(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	mustJobTransition(t, j, Assessing)
	if err := j.SetNext(Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := j.Transition(Extracting); !errors.Is(err, ErrCrossRequired) {
		t.Errorf("Transition(Extracting), error = %v, want ErrCrossRequired", err)
	}
	if j.State().State != Assessing {
		t.Error("the refused Transition moved the attempt anyway")
	}
}

// TestJob_FinishYieldsTheLease pins §3.9's largest leak: revision 3's Finish
// yielded nothing, so every pre-boundary failure, every Unrecoverable verdict
// and every cancel lost a pool-A slot until restart.
func TestJob_FinishYieldsTheLease(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	l := NewLease(1)
	if err := j.Grant(l); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	got, err := j.Finish(OutcomeFailed, testClock())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got != l {
		t.Errorf("Finish yielded %p, want the held lease %p", got, l)
	}
	if j.HoldsLease() {
		t.Error("the job still holds a lease after settling")
	}
}

// TestJob_CrossAndFinishDoNotDeadlock is the pin for the reason surrenderLocked
// exists. Cross and Finish run their bodies inside withOpenAttempt's callback,
// which holds j.mu while they mutate the attempt and yield the lease;
// sync.RWMutex is not reentrant, so either one calling the exported
// Surrender() — which takes j.mu itself — would hang the job permanently, with
// no error and no timeout. The hazard is unchanged by which frame takes the
// lock; what matters is that it is held when the callback runs. A deadlocked
// test does not fail, it hangs, so this runs under a watchdog.
func TestJob_CrossAndFinishDoNotDeadlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Job) error
	}{
		{"Finish", func(j *Job) error { _, err := j.Finish(OutcomeFailed, testClock()); return err }},
		{"Cross", func(j *Job) error {
			// Setup errors are RETURNED, not raised with t.Fatalf. This
			// closure runs on the spawned goroutine below, and t.Fatalf from
			// a goroutine other than the test's own calls runtime.Goexit on
			// that goroutine alone: nothing would be sent on done, the 5s
			// watchdog would fire, and its message positively asserts a
			// diagnosis — "almost certainly taking j.mu twice" — that would
			// be wrong. A setup failure must travel the same channel as a
			// door failure so the watchdog only ever speaks about a hang.
			if err := j.Transition(Assessing); err != nil {
				return fmt.Errorf("setup: Transition(Assessing): %w", err)
			}
			if err := j.SetNext(Extracting); err != nil {
				return fmt.Errorf("setup: SetNext(Extracting): %w", err)
			}
			_, err := j.Cross(Extracting)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := newTestJob(t)
			mustBegin(t, j)
			if err := j.Grant(NewLease(1)); err != nil {
				t.Fatalf("Grant: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- tc.run(j) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not return within 5s — it is almost certainly taking j.mu twice; "+
					"the doors must call surrenderLocked, not the exported Surrender", tc.name)
			}
		})
	}
}

// TestAttempt_ActivityClearsOnEveryStateChange covers all three mutators that
// assign a.state, not just one.
//
// Its predecessor drove only cross — commit c84b3fa6 changed the call from
// mustTransition to mustCross when the boundary got its own door, and the test
// went on being named for transition. Commenting out `a.activity = ActNone` in
// transition AND in finish left the whole package green, so two of the three
// writers were unasserted while a test named for one of them passed.
func TestAttempt_ActivityClearsOnEveryStateChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(t *testing.T, a *Attempt)
	}{
		{"transition", func(t *testing.T, a *Attempt) { mustTransition(t, a, Repairing) }},
		{"cross", func(t *testing.T, a *Attempt) {
			if err := a.setNext(Extracting); err != nil {
				t.Fatalf("setNext: %v", err)
			}
			mustCross(t, a, Extracting)
		}},
		{"finish", func(t *testing.T, a *Attempt) {
			if err := a.finish(OutcomeFailed, testClock()); err != nil {
				t.Fatalf("finish: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAttempt(testClock())
			mustTransition(t, &a, Assessing)
			a.setActivity(ActPar2Verify)
			if got := a.view().Activity; got != ActPar2Verify {
				t.Fatalf("Activity = %v, want ActPar2Verify", got)
			}
			tc.act(t, &a)
			if got := a.view().Activity; got != ActNone {
				t.Errorf("Activity = %v after %s, want ActNone — the state changed, so what was executing is over", got, tc.name)
			}
		})
	}
}

// TestAttempt_RejectsTheSentinelAsDestination discriminates the sentinel guard
// from the graph guard.
//
// Asserting only "some error" cannot tell them apart: StateUnset is in no
// state's legalEdges, so CanTransition rejects it too, and deleting BOTH
// sentinel guards left the package green. Matching the message is what makes
// the guard's own removal visible.
func TestAttempt_RejectsTheSentinelAsDestination(t *testing.T) {
	const want = "StateUnset is not a destination"
	for _, tc := range []struct {
		name string
		call func(a *Attempt) error
	}{
		{"transition", func(a *Attempt) error { return a.transition(StateUnset) }},
		{"setNext", func(a *Attempt) error { return a.setNext(StateUnset) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAttempt(testClock())
			err := tc.call(&a)
			if !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("%s(StateUnset) = %v, want ErrIllegalTransition", tc.name, err)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s(StateUnset) = %q, want it to name the sentinel (%q); "+
					"a bare ErrIllegalTransition would also come from CanTransition, "+
					"so this assertion is what pins the sentinel guard itself", tc.name, err, want)
			}
		})
	}
}

// TestAttempt_CrossWithoutARecordedDestination pins that cross distinguishes
// "nothing recorded" from "something else recorded". Falling through to the
// mismatch arm formats "StateUnset is recorded", asserting a verdict setNext
// refuses to store.
func TestAttempt_CrossWithoutARecordedDestination(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	err := a.cross(Extracting)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("cross with no verdict = %v, want ErrIllegalTransition", err)
	}
	if strings.Contains(err.Error(), "StateUnset is recorded") {
		t.Errorf("cross reports %q — StateUnset is never recorded; setNext rejects it outright", err)
	}
	if !strings.Contains(err.Error(), "no destination is recorded") {
		t.Errorf("cross = %q, want it to say no destination is recorded", err)
	}
}

// TestAttempt_TransitionRejectsNonEdgesIntoProduction pins the guard ORDER.
//
// With the boundary test ahead of CanTransition, every Correctness→Production
// pair answered ErrCrossRequired — "call Cross instead" — including four pairs
// that are not edges at all. A caller obeying that got ErrIllegalTransition
// from cross, which is legal only from Assessing. Only the real edge may reach
// the boundary guard.
func TestAttempt_TransitionRejectsNonEdgesIntoProduction(t *testing.T) {
	for _, tc := range []struct {
		name string
		from State
		to   State
	}{
		{"Fetching to Extracting", Fetching, Extracting},
		{"Fetching to Finalizing", Fetching, Finalizing},
		{"Repairing to Extracting", Repairing, Extracting},
		{"Repairing to Finalizing", Repairing, Finalizing},
		{"Assessing to Finalizing", Assessing, Finalizing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAttempt(testClock())
			if tc.from != Fetching {
				mustTransition(t, &a, Assessing)
			}
			if tc.from == Repairing {
				mustTransition(t, &a, Repairing)
			}
			err := a.transition(tc.to)
			if errors.Is(err, ErrCrossRequired) {
				t.Errorf("transition(%v) from %v = ErrCrossRequired, telling the caller to use Cross — "+
					"but %v→%v is not an edge, and cross would refuse it too", tc.to, tc.from, tc.from, tc.to)
			}
			if !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("transition(%v) from %v = %v, want ErrIllegalTransition", tc.to, tc.from, err)
			}
		})
	}
	// The one real crossing edge still routes to Cross.
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	if err := a.transition(Extracting); !errors.Is(err, ErrCrossRequired) {
		t.Errorf("transition(Extracting) from Assessing = %v, want ErrCrossRequired — reordering must not lose the real edge", err)
	}
}

// TestAttempt_TransitionReportsTheRecordedVerdict pins that a refusal caused
// by next says so, rather than naming a pair that is a legal edge.
func TestAttempt_TransitionReportsTheRecordedVerdict(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	if err := a.setNext(Repairing); err != nil {
		t.Fatalf("setNext: %v", err)
	}
	err := a.transition(Fetching)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("transition(Fetching) with Repairing recorded = %v, want ErrIllegalTransition", err)
	}
	if !strings.Contains(err.Error(), "Repairing is recorded") {
		t.Errorf("transition = %q, want it to name the recorded verdict; Assessing → Fetching IS a "+
			"legal edge (transition.go), so reporting it as an illegal edge hides the real reason", err)
	}
}

// TestAttempt_FinishRefusesOKBeforeFinalizing pins the guard AND the reopen it
// prevents. Without it, Finish(OutcomeOK) from Fetching settles the attempt
// while the position still reads Fetching, and BeginAttempt — which refuses a
// reopen only for an attempt that crossed — opens a second attempt on a job
// already declared complete.
//
// Extracting is in the list, and it is the case that is NOT obvious. Extracting
// is past the boundary, so a guard phrased as "must have crossed" admits it.
// But §3.3's work-spine table gives Extracting exactly one completion —
// SetNext(Finalizing) — and Finish(OutcomeOK) is Finalizing's row alone.
// OutcomeOK means "the job produced its files"; at Extracting the archives have
// been unpacked into the working directory and nothing has been moved to the
// destination or run a user script. Settling OK there reports a job complete
// with its output stranded in a temporary unpack directory.
func TestAttempt_FinishRefusesOKBeforeFinalizing(t *testing.T) {
	for _, from := range []State{Fetching, Assessing, Repairing, Extracting} {
		t.Run(from.String(), func(t *testing.T) {
			a := newAttempt(testClock())
			if from != Fetching {
				mustTransition(t, &a, Assessing)
			}
			if from == Repairing {
				mustTransition(t, &a, Repairing)
			}
			if from == Extracting {
				mustCross(t, &a, Extracting)
			}
			if err := a.finish(OutcomeOK, testClock()); !errors.Is(err, ErrInvalidOutcome) {
				t.Errorf("finish(OutcomeOK) from %v = %v, want ErrInvalidOutcome", from, err)
			}
		})
	}
	// Every state EXCEPT Finalizing must appear above. Stated as a count
	// against AllStates() rather than as a list, so adding a state fails here
	// instead of quietly leaving the new one unguarded.
	t.Run("the table covers every state but Finalizing", func(t *testing.T) {
		if want := len(AllStates()) - 1; want != 4 {
			t.Fatalf("AllStates() has %d members, so the loop above should test %d states, not 4", len(AllStates()), want)
		}
	})
	t.Run("a job settled OK cannot reopen", func(t *testing.T) {
		j := newTestJob(t)
		mustBegin(t, j)
		mustJobTransition(t, j, Assessing)
		if err := j.SetNext(Extracting); err != nil {
			t.Fatalf("SetNext: %v", err)
		}
		if _, err := j.Cross(Extracting); err != nil {
			t.Fatalf("Cross: %v", err)
		}
		if err := j.SetNext(Finalizing); err != nil {
			t.Fatalf("SetNext(Finalizing): %v", err)
		}
		mustJobTransition(t, j, Finalizing)
		if _, err := j.Finish(OutcomeOK, testClock()); err != nil {
			t.Fatalf("Finish(OutcomeOK) from Finalizing: %v", err)
		}
		if err := j.BeginAttempt(testClock()); err == nil {
			t.Error("BeginAttempt reopened a job that produced its files")
		}
	})
}

// TestJob_FailedCrossAndFinishKeepTheLease pins that the lease is yielded only
// on the SUCCESS path of each door.
//
// Moving surrenderLocked ahead of the error check in Cross left the package
// green: no subtest of the door-error tests granted a lease, so a regression
// that stripped a job's pool-A admission token while stranding it in Assessing
// was invisible. (*Lease, error) must never mean "failed, and here is a lease".
func TestJob_FailedCrossAndFinishKeepTheLease(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(t *testing.T, j *Job) error
	}{
		{"Cross with no verdict recorded", func(t *testing.T, j *Job) error {
			mustJobTransition(t, j, Assessing)
			_, err := j.Cross(Extracting)
			return err
		}},
		{"Cross from a non-Assessing state", func(t *testing.T, j *Job) error {
			_, err := j.Cross(Extracting)
			return err
		}},
		{"Finish with an unrecognized outcome", func(t *testing.T, j *Job) error {
			_, err := j.Finish(Outcome(200), testClock())
			return err
		}},
		{"Finish(OutcomeOK) before the boundary", func(t *testing.T, j *Job) error {
			_, err := j.Finish(OutcomeOK, testClock())
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := newTestJob(t)
			mustBegin(t, j)
			if err := j.Grant(NewLease(1)); err != nil {
				t.Fatalf("Grant: %v", err)
			}
			if err := tc.call(t, j); err == nil {
				t.Fatalf("%s returned no error; this case must fail for the assertion below to mean anything", tc.name)
			}
			if !j.HoldsLease() {
				t.Errorf("%s surrendered the lease on an error path; the job is still in Correctness and still needs it", tc.name)
			}
		})
	}
}

// TestAttempt_CrossReportsTheRecordedVerdictBeforeTheGraph pins cross's check
// ORDER against transition's, which documents the principle: a recorded
// verdict refuses a different destination BEFORE the graph is consulted,
// because naming the pair hides the real reason.
//
// cross had the two inverted, so an Assessing attempt holding next=Extracting
// answered cross(Fetching) with "Assessing → Fetching is an edge, but
// transition takes it, not cross". That is true and it is the wrong advice:
// the caller does as told, calls transition(Fetching), and is refused again
// because next says Extracting. Two errors to learn one fact.
//
// The third subtest is the reason this is NOT a wholesale reorder. Moving the
// next == StateUnset check up as well would answer "no destination is
// recorded" for a pair that is not an edge at all, which is the same class of
// mistake in the other direction. transition does not have that problem
// because its verdict check is guarded by next != StateUnset and so only fires
// when a verdict actually exists; cross now matches that shape exactly.
func TestAttempt_CrossReportsTheRecordedVerdictBeforeTheGraph(t *testing.T) {
	t.Run("a recorded verdict outranks the door", func(t *testing.T) {
		a := newAttempt(testClock())
		mustTransition(t, &a, Assessing)
		if err := a.setNext(Extracting); err != nil {
			t.Fatalf("setNext: %v", err)
		}
		err := a.cross(Fetching)
		if !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("cross(Fetching) = %v, want ErrIllegalTransition", err)
		}
		if !strings.Contains(err.Error(), "Extracting is recorded") {
			t.Errorf("cross(Fetching) = %q, want it to name the recorded verdict; sending the "+
				"caller to transition instead just earns them a second refusal from the "+
				"to == next check there", err)
		}
		if errors.Is(err, ErrTransitionRequired) {
			t.Errorf("cross(Fetching) = %v, reported as a wrong-door error; the door is not "+
				"the caller's problem here, the recorded verdict is", err)
		}
	})
	t.Run("a non-edge is still reported as a non-edge", func(t *testing.T) {
		a := newAttempt(testClock())
		err := a.cross(Fetching) // Fetching → Fetching is not an edge at all
		if !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("cross(Fetching) from Fetching = %v, want ErrIllegalTransition", err)
		}
		if strings.Contains(err.Error(), "no destination is recorded") {
			t.Errorf("cross(Fetching) from Fetching = %q; the missing verdict is not the "+
				"reason — this pair is not an edge, and saying otherwise sends the caller "+
				"off to SetNext for a move that would still be refused", err)
		}
	})
	t.Run("a missing verdict is still reported on a real cross edge", func(t *testing.T) {
		a := newAttempt(testClock())
		mustTransition(t, &a, Assessing)
		err := a.cross(Extracting) // the byCross edge, but nothing recorded
		if !strings.Contains(err.Error(), "no destination is recorded") {
			t.Errorf("cross(Extracting) with next unset = %q, want it to name the missing "+
				"verdict", err)
		}
	})
}
