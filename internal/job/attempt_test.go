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

// TestAttempt_TransitionRejectsWaiting pins that hold is the only door into
// Waiting. Before this check existed, Extracting -> transition(Waiting) ->
// transition(Fetching) reached Fetching with err=nil at both hops — the
// second hop was later closed by the a.next check below, but the first hop
// itself stayed open, and taking it stranded the attempt: transition's own
// a.next check then accepted nothing but Waiting itself as a destination,
// and hold refuses to re-park an attempt that is already Waiting, so only
// finish could still move it. Confirmed via a probe:
// Fetching -> transition(Waiting) => state=Waiting next=Waiting reason=NoLease,
// then hold(Assessing) and transition(Assessing) both refused, finish the
// only move left.
func TestAttempt_TransitionRejectsWaiting(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	mustTransition(t, &a, Extracting)
	if err := a.transition(Waiting); !errors.Is(err, ErrHoldRequired) {
		t.Fatalf("transition(Waiting) error = %v, want ErrHoldRequired", err)
	}
	if got := a.view().State; got != Extracting {
		t.Errorf("State = %v after a rejected transition, want Extracting unchanged", got)
	}
}

// TestAttempt_BoundaryHoldsAcrossAHold pins the two-hop escape
// TestBoundaryIsOneWay (transition_test.go) cannot see, now exercised through
// hold — the only door into Waiting, per TestAttempt_TransitionRejectsWaiting
// above. That test enumerates direct edges in legalEdges, but
// Production -> Waiting -> Correctness is two individually legal edges — a
// pause into Waiting, then an unconstrained resume out of it — composing
// into the exact edge the graph forbids directly. This confirms the
// boundary survives the round trip through the legitimate door, not only
// through the one transition itself now refuses.
func TestAttempt_BoundaryHoldsAcrossAHold(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	mustTransition(t, &a, Extracting)
	if err := a.hold(Finalizing, NoComputeSlot); err != nil {
		t.Fatalf("hold(Finalizing): %v", err)
	}
	if err := a.transition(Fetching); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("transition(Fetching) after Production paused, error = %v, want ErrIllegalTransition", err)
	}
	if got := a.view().State; got == Fetching {
		t.Errorf("State = %v, want NOT Fetching; Production must not resume into Correctness", got)
	}
}

// TestAttempt_TransitionFromWaitingRequiresNext pins the other half of the
// same fix: next is validated when hold is taken and then must be honored at
// resume, not merely consulted for its own sake. Before this check existed,
// hold(next=Assessing) followed by transition(Repairing) reached Repairing
// with err=nil — the guard hold installs on next was real, but nothing
// re-checked it one call later.
func TestAttempt_TransitionFromWaitingRequiresNext(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.hold(Assessing, NoComputeSlot); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := a.transition(Repairing); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("transition(Repairing) error = %v, want ErrIllegalTransition; next was Assessing", err)
	}
	if got := a.view().State; got != Waiting {
		t.Errorf("State = %v after a rejected resume, want Waiting unchanged", got)
	}
	if err := a.transition(Assessing); err != nil {
		t.Fatalf("transition(Assessing), the declared next, should succeed: %v", err)
	}
	// Leaving Waiting still clears next/reason to their zero values — this
	// resume took the declared path rather than being refused, so it is the
	// case that exercises the clearing lines rather than skipping them.
	v := a.view()
	if v.Next != Waiting || v.Reason != NoLease {
		t.Errorf("view = %+v after a successful resume; want Next=Waiting Reason=NoLease cleared", v)
	}
}

// TestAttempt_HoldRejectsWhenAlreadyWaiting pins the third path into the same
// hole: hold does not require transition through Waiting at all, so it needs
// its own guard against being called twice. Confirmed reachable before this
// check existed: Extracting -> hold(Finalizing) -> hold(Fetching) ->
// transition(Fetching) succeeded, even with the transition-side next check in
// place, because the second hold silently overwrote next before transition
// ever saw the first one.
func TestAttempt_HoldRejectsWhenAlreadyWaiting(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	mustTransition(t, &a, Extracting)
	if err := a.hold(Finalizing, NoComputeSlot); err != nil {
		t.Fatalf("hold(Finalizing): %v", err)
	}
	if err := a.hold(Fetching, NoComputeSlot); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("second hold error = %v, want ErrIllegalTransition; a waiting attempt cannot re-declare its destination", err)
	}
	if got := a.view().Next; got != Finalizing {
		t.Errorf("Next = %v after a rejected second hold, want Finalizing unchanged", got)
	}
}

// TestAttempt_HoldRejectsResumeIntoFinished pins that finish stays the sole
// door into Finished even via a hold: without this, hold(Assessing) followed
// by transition(Repairing) could not reach Finished directly, but nothing
// stopped hold(Finished, ...) itself from parking an attempt with a destination
// finish never validated.
func TestAttempt_HoldRejectsResumeIntoFinished(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.hold(Finished, NoLease); !errors.Is(err, ErrFinishRequired) {
		t.Errorf("hold(Finished) error = %v, want ErrFinishRequired", err)
	}
	if got := a.view().State; got != Fetching {
		t.Errorf("State = %v after a rejected hold, want Fetching unchanged", got)
	}
}

// TestAttempt_HoldRejectsResumeIntoWaiting pins that next == Waiting is
// refused as self-referential: there is nothing to resume into.
func TestAttempt_HoldRejectsResumeIntoWaiting(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.hold(Waiting, NoLease); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("hold(Waiting) error = %v, want ErrIllegalTransition", err)
	}
	if got := a.view().State; got != Fetching {
		t.Errorf("State = %v after a rejected hold, want Fetching unchanged", got)
	}
}

// TestAttempt_HoldRejectsAfterFinish pins that hold refuses once an attempt
// is Finished. next is deliberately NOT Finished or Waiting (Extracting
// instead), so this isolates hold's CanTransition(a.state, next) check from
// the newer next == Finished / next == Waiting checks above it.
//
// Writing this test is what surfaced that hold used to carry a second,
// separate CanTransition(a.state, Waiting) check ahead of this one — and
// that it was dead: it could only ever fire when a.state == Finished, and by
// then next == Finished was already excluded, so CanTransition(Finished,
// next) below was always false too, for the same reason, one line down. That
// check has been removed; this test now pins the single check that replaced
// it, rather than a first-of-two check that never independently rejected
// anything.
func TestAttempt_HoldRejectsAfterFinish(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.finish(OutcomeOK, testClock()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := a.hold(Extracting, NoComputeSlot); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("hold(Extracting) after finish, error = %v, want ErrIllegalTransition", err)
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

// TestAttempt_FinishedNeverOpen pins the invariant TestAttempt_
// TransitionRejectsFinished exists to protect: no reachable sequence of
// mutators leaves state == Finished while isOpen() still reports true.
// finish is the only mutator that can reach Finished, and it always assigns
// Outcome in the same call that assigns state, so the two can never come
// apart — and the direct route through transition is rejected outright,
// even once the attempt is already Finished, rather than merely happening
// not to produce the hole in this particular sequence.
func TestAttempt_FinishedNeverOpen(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	mustTransition(t, &a, Extracting)
	mustTransition(t, &a, Finalizing)
	if err := a.finish(OutcomeOK, testClock()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if a.view().State == Finished && a.isOpen() {
		t.Fatal("state == Finished but isOpen() == true; outcome and state disagree")
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
	if !errors.Is(err, ErrOutcomeAlreadySet) {
		t.Fatalf("second finish (crossed + settled) error = %v, want ErrOutcomeAlreadySet", err)
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

// TestAttempt_FinishClearsNextAndReason pins that finish clears next and
// reason along with activity. Before this fix, a cancel taken while paused
// left them at the hold's stale values — {State:Finished, Next:Assessing,
// Reason:UserPaused} — contradicting StateView's contract that Next and
// Reason are meaningful only when State is Waiting.
func TestAttempt_FinishClearsNextAndReason(t *testing.T) {
	a := newAttempt(testClock())
	if err := a.hold(Assessing, UserPaused); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := a.finish(OutcomeCancelled, testClock()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	v := a.view()
	if v.Next != Waiting || v.Reason != NoLease {
		t.Errorf("view = %+v after finish; want Next=Waiting Reason=NoLease, not the stale hold values", v)
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

// TestAttempt_FinishRejectsUnrecoverableAfterCrossingThenHold pins that the
// guard in finish tracks the latch (a.crossed), not the transient state: hold
// sets a.state to Waiting, so a guard reading IsProduction(a.state) would see
// a held attempt as Correctness-zone and let Unrecoverable through even
// though the attempt already crossed the boundary in this same attempt.
// Probe (pre-fix): an attempt that transitions into Extracting (Production,
// crossed latches true), then hold(Finalizing, ...) (state becomes Waiting),
// then finish(OutcomeUnrecoverable) returned err=nil — recording a verdict
// that means "never crossed" on an attempt that had.
func TestAttempt_FinishRejectsUnrecoverableAfterCrossingThenHold(t *testing.T) {
	a := newAttempt(testClock())
	mustTransition(t, &a, Assessing)
	mustTransition(t, &a, Extracting)
	if err := a.hold(Finalizing, NoComputeSlot); err != nil {
		t.Fatalf("hold(Finalizing): %v", err)
	}
	if got := a.view().State; got != Waiting {
		t.Fatalf("State = %v after hold, want Waiting", got)
	}
	err := a.finish(OutcomeUnrecoverable, testClock())
	if !errors.Is(err, ErrUnrecoverableAfterBoundary) {
		t.Fatalf("finish(Unrecoverable) while held after crossing, error = %v, want ErrUnrecoverableAfterBoundary", err)
	}
	if got := a.view(); got.State == Finished || got.Outcome != OutcomePending {
		t.Errorf("view = %+v after a rejected finish, want unchanged (Waiting, Pending)", got)
	}
}

// TestAttempt_FinishSucceedsFromAnyOpenState pins that finish reaches
// Finished from every non-terminal state reachable via legal transitions, not
// only from one — needed because finish() has no CanTransition(state,
// Finished) guard of its own: CanTransition(s, Finished) is true for every s
// in AllStates() (legalEdges gives every non-terminal state a cancel edge),
// so such a guard could never reject anything and was removed rather than
// kept as an inert check. This table IS the coverage that removal relies on,
// so it walks all six non-terminal states rather than one — Waiting is the
// boundary case a single-state version of this test previously omitted, and
// is reached via hold rather than transition since transition cannot produce
// it directly from Fetching.
func TestAttempt_FinishSucceedsFromAnyOpenState(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, a *Attempt)
	}{
		{"Fetching", func(t *testing.T, a *Attempt) {}},
		{"Waiting", func(t *testing.T, a *Attempt) {
			if err := a.hold(Assessing, NoComputeSlot); err != nil {
				t.Fatalf("hold: %v", err)
			}
		}},
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
