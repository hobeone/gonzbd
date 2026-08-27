package job

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// errOutcomeAlreadySet is returned when a settled attempt is finished again.
// The write-once rule is enforced here rather than by convention, because a
// second assignment is exactly the mutation the design exists to prevent.
//
// Unexported: this error is reachable only from an in-package caller of
// Attempt.finish directly (e.g. this package's own tests). Job.Finish checks
// !a.isOpen() itself, before calling this method, and returns ErrNoOpenAttempt
// on a settled attempt — a strict superset of this check, since isOpen() is
// defined as outcome == OutcomePending. (Finish gets that check from
// withOpenAttempt, via the withOpenAttemptLease adapter that lets a door
// return the lease it yields.) No external caller can
// reach this branch through the public API, so exporting it would be a dead
// sentinel.
var errOutcomeAlreadySet = errors.New("job: attempt outcome already set")

// ErrUnrecoverableAfterBoundary is returned when finish is asked to record
// OutcomeUnrecoverable for an attempt that has crossed into Production
// (a.crossed), whether or not it is still there — finish is about to
// overwrite a.state in the same call, which used to erase the
// Production state the attempt actually crossed at, so the guard tracks the
// latch rather than the transient state. D3 defines OutcomeUnrecoverable
// as "the job never crossed the boundary" specifically so its files stay in
// the working directory and the job stays retryable — a verdict finish must
// not let contradict where the attempt actually crossed. This is a sentinel
// rather than a plain fmt.Errorf: a caller that reaches Production and then
// gets an Unrecoverable verdict from downstream (e.g. par2 misclassifying a
// Production-stage fault) has a caller bug to fix, not a job to fail, and
// distinguishing that case with errors.Is is the whole reason to name it.
var ErrUnrecoverableAfterBoundary = errors.New("job: cannot record Unrecoverable for an attempt past the Correctness/Production boundary")

// ErrInvalidOutcome is returned when finish is asked to record a verdict
// that is not a legitimate outcome the machine produces — either
// OutcomePending (not a verdict at all) or a value AllOutcomes() does not
// declare. Exported, unlike errOutcomeAlreadySet: an external caller of
// Job.Finish can pass an arbitrary Outcome and observe this branch directly
// (Finish's own !a.isOpen() guard does not shadow it the way it shadows the
// write-once case), so a caller distinguishing "bad argument" from other
// finish failures needs a sentinel to match against.
var ErrInvalidOutcome = errors.New("job: invalid outcome")

// ErrNextAlreadySet is returned by setNext when a different destination is
// already recorded. This is defect 3's guard, carried into the door that
// replaced hold: without it a verdict of Repairing could be overwritten with
// Extracting and the job would cross the boundary skipping repair.
var ErrNextAlreadySet = errors.New("job: next is already set to a different destination")

// ErrCrossRequired is returned when transition is asked to take the one
// Correctness -> Production edge. Cross is the sole door across the
// irreversible boundary, because entering Production and surrendering the
// lease must happen together — see Job.Cross.
var ErrCrossRequired = errors.New("job: transition cannot cross the boundary; call Cross instead")

// Attempt is one run of a job through the machine. The state machine lives
// here, not on Job: a job has a LIST of attempts, each carrying its own
// write-once Outcome, so a retry appends a verdict rather than revising one
// (spec §3.1).
//
// An attempt opens when BeginAttempt is called and no attempt is open, and
// closes when finish assigns its verdict. It opens holding nothing: D-I12 decoupled
// opening from the lease, so a job can have a live attempt in Fetching with
// j.lease still nil (see newAttempt below). Pause and resume inside an attempt do not
// end it — the lease is surrendered and later re-taken, and the attempt
// persists across that.
//
// Every field is unexported and every mutator is package-private. Job is the
// only NON-TEST caller — this package's own tests drive a.transition, a.cross,
// a.setNext and a.finish directly, which is how the attempt-level guards get
// exercised without a Job around them. Job holds its own lock across each of
// these; Attempt does no locking of its own, so an in-package caller that is
// not Job is responsible for whatever synchronisation it needs.
type Attempt struct {
	state State
	// next records that this state's work has ENDED and names where the job
	// continues to; setNext, below, documents what it means and the guards on
	// writing it. Four functions assign it, and only these four: setNext sets
	// it, and transition, cross and finish each clear it on the same call that
	// writes a.state (`git grep -n 'nex[t] = ' -- 'internal/job/*.go'
	// ':!internal/job/*_test.go'` returns exactly four lines, one in each of
	// those bodies; the bracket keeps this citation from matching its own
	// text). TWO mutators assign a.state after construction — transition and
	// cross (`git grep -n 'stat[e] = ' -- 'internal/job/*.go'
	// ':!internal/job/*_test.go'` returns exactly those two lines). finish
	// used to be a third and is not any more: change 03 stopped it
	// overwriting the position, which is what let the crossed latch be
	// deleted. It still clears next, which is why the next enumeration above
	// names four functions and this one names two.
	//
	// newAttempt is a construction site, not a mutator: it builds
	// Attempt{state: Fetching} with next left at its StateUnset zero, which is
	// already the cleared value.
	//
	// TestNextWrites_MatchTheEnumerationStatedInProse fails if the next
	// enumeration above goes stale; a.state has no such test.
	next     State
	activity Activity
	// outcome is the attempt's verdict, and its being set is what "settled"
	// means — isOpen() is defined as outcome == OutcomePending, so this field
	// alone decides whether the attempt is still live. There is no Finished
	// state; a settled attempt keeps the position it settled at.
	//
	// finish is the only mutator that assigns it, and that is enforced rather
	// than asserted: TestOutcomeWrites_MatchTheEnumerationStatedInProse
	// enumerates every site in this package's non-test sources that sets this
	// field — a plain `=` assignment and an `outcome: x` composite-literal key
	// alike — and requires finish be the only one. The claim lived on
	// ErrFinishRequired until change 03 deleted both that error and the
	// Finished state it guarded; transition can no longer be asked to reach
	// settledness, because there is no longer a State value naming it, so what
	// was a runtime error is now a compile error.
	//
	// What the enumeration guarantees is the property that error protected:
	// an attempt cannot settle without being assigned a settled Outcome in the
	// same call, so isOpen() cannot report an attempt open when nothing is
	// ever going to close it.
	outcome  Outcome
	assessed bool
	// started and ended have no reader yet in this package outside their own
	// tests — TestNewAttempt_StartsFetching checks started, and
	// TestAttempt_FinishIsWriteOnce checks ended (`git grep -n
	// '\.ended\|\.started' internal/job/*_test.go` returns exactly these
	// four lines). That is expected for a package built ahead of its
	// consumers (see doc.go, "What this package does not do"). The next
	// plan's history/durability surface is the intended consumer:
	// per-attempt start/end timestamps are what a
	// retried job's history entry needs to show each attempt's own duration,
	// rather than only the job's.
	started time.Time
	ended   time.Time
}

// newAttempt opens an attempt in Fetching, holding nothing. There is no arm
// for opening in any other state — BeginAttempt is the sole NON-TEST caller
// and always starts here (`git grep -n 'newAttemp[t](' -- 'internal/job/*.go'
// ':!internal/job/*_test.go'` returns two lines, this declaration and
// BeginAttempt's call in job.go — no line number, because the last one was
// stale by the next commit; the grep is the durable form. This package's own
// tests construct
// attempts directly, which is how the attempt-level guards get exercised
// without a Job around them) — and no lease is required to reach it: D-I12 makes Fetching
// holding nothing a legal, representable state (a paused or restarted fetch
// looks exactly like this), so requiring one to open the attempt would
// contradict the model that makes those two states expressible.
func newAttempt(now time.Time) Attempt {
	return Attempt{state: Fetching, started: now}
}

func (a *Attempt) isOpen() bool { return a.outcome == OutcomePending }

// crossed reports whether this attempt reached Production. It is DERIVED from
// the position, not stored.
//
// It used to be a latch: a bool that cross set true and nothing ever cleared.
// The latch existed for one reason — finish overwrote a.state with a Finished
// value, erasing where the attempt had got to, so the position could no longer
// answer. finish no longer does that, so the position answers directly and the
// shadow field is gone.
//
// What this costs, stated plainly because it is a real trade. A latch is
// independent of the graph; this is not. The two agree only because no edge
// runs from Production back to Correctness — add one, and a job could cross,
// return, and report crossed() false, at which point BeginAttempt would reopen
// a job that has already written files. So the reopen guard now DEPENDS on the
// boundary invariant instead of being a second, independent mechanism
// enforcing it.
//
// That dependency is deliberate and covered rather than merely assumed:
// TestBoundaryIsOneWay pins the absence of a reverse edge directly, and
// TestBoundaryIsUnreachableByAnyPath walks every reachable configuration and
// asserts that any job which has been in Production still reports crossed()
// true. The second is the one that matters — it re-derives the property from
// replayed history rather than from this function, so it does not agree with
// this function by construction.
func (a *Attempt) crossed() bool { return IsProduction(a.state) }

func (a *Attempt) view() StateView {
	return StateView{
		State:    a.state,
		Next:     a.next,
		Activity: a.activity,
		Outcome:  a.outcome,
		Assessed: a.assessed,
	}
}

// setNext records that this state's work has ENDED and where the job continues
// to. It is the marker Waiting used to carry, and the fact it carries is not
// derivable: "has this download finished?" is about the world, not the graph.
//
// Ended, not succeeded — a Fetching that exhausts every server has ended, and
// Assessing decides what that means. Work that ends without continuing settles
// via finish instead and leaves next unset, which is why Finalizing never sets
// it and is not an exception to the rule.
//
// Three guards, each closing a specific hole:
//   - the destination must be a legal edge from the current state;
//   - the sentinel is not a destination;
//   - write-once per visit: a DIFFERENT destination is refused, the same one
//     is a no-op. transition clears next when it takes the move, so re-entering
//     a state permits a fresh verdict.
func (a *Attempt) setNext(n State) error {
	if n == StateUnset {
		return fmt.Errorf("%w: StateUnset is not a destination", ErrIllegalTransition)
	}
	if !CanTransition(a.state, n) {
		return illegalTransition(a.state, n)
	}
	if a.next != StateUnset && a.next != n {
		return fmt.Errorf("%w: %s is recorded, refusing to replace it with %s", ErrNextAlreadySet, a.next, n)
	}
	a.next = n
	return nil
}

// transition moves the attempt to `to`. Activity is cleared, because it
// describes the state being left. next is cleared, because the move consumes it.
//
// Refuses the one Correctness -> Production edge outright (ErrCrossRequired):
// crossing must surrender the lease in the same call, which only Cross can
// do, so transition is not a door onto that edge at all — not even when next
// already names it.
//
// When next is SET for any edge transition still permits, to must equal it.
// Its purpose is enforcing that once a state's work has decided where to go,
// nothing else may choose. From Assessing, legalEdges also permits Fetching
// and Repairing, so without this check a caller could ignore a NeedsRepair
// verdict and take Fetching or Repairing instead.
//
// When next is UNSET the edge map alone decides, which is the ordinary
// forward move of a state that has just started.
func (a *Attempt) transition(to State) error {
	if to == StateUnset {
		return fmt.Errorf("%w: StateUnset is not a destination", ErrIllegalTransition)
	}
	// A recorded verdict refuses a different destination BEFORE the graph is
	// consulted, and says so. Reporting illegalTransition here would name a
	// pair that is often perfectly legal — Assessing → Fetching is an edge —
	// and hide the real reason, which is that next already says Repairing.
	if a.next != StateUnset && to != a.next {
		return fmt.Errorf("%w: %s is recorded; transition cannot take %s instead", ErrIllegalTransition, a.next, to)
	}
	// One lookup answers both questions, and they cannot disagree: does this
	// move exist, and who may take it. There is no zone predicate here to put
	// in the wrong order — the earlier bug was a separate boundary test that
	// ran before the graph, so every Correctness→Production PAIR answered
	// ErrCrossRequired, including the four that are not edges at all.
	//
	// These two checks are disjoint by construction: an edge either exists or
	// it does not, and if it exists it carries exactly one door. The verdict
	// check above is NOT disjoint from them — a recorded verdict and a
	// non-edge can both be true — and is deliberately first because it names
	// the more specific reason.
	e, ok := edgeFrom(a.state, to)
	if !ok {
		return illegalTransition(a.state, to)
	}
	if e.door != byTransition {
		return ErrCrossRequired
	}
	a.state = to
	a.activity = ActNone
	a.next = StateUnset
	if to == Assessing {
		a.assessed = true
	}
	return nil
}

// cross moves the attempt across the irreversible boundary. Must hold j.mu;
// the lease is released by the CALLER through surrenderLocked, which is why
// this returns nothing about it.
//
// It validates exactly what transition would have, and both checks matter:
// without them Cross would be a hole in the single-decider property that
// transition's to == next check exists to protect — a caller could cross from
// anywhere, to anywhere in Production, ignoring the verdict Assessing recorded.
func (a *Attempt) cross(to State) error {
	// The same single lookup transition uses. It replaces three guards that
	// each re-derived a fact legalEdges already holds: "to must be a
	// Production state", "a.state must be Assessing", and a CanTransition
	// check that was unreachable because the first two implied it. There is
	// exactly one byCross edge, so being on it IS being at Assessing headed
	// for Extracting — the door check subsumes the state check rather than
	// duplicating it.
	e, ok := edgeFrom(a.state, to)
	if !ok {
		return illegalTransition(a.state, to)
	}
	if e.door != byCross {
		return wrongDoor(a.state, to)
	}
	// Nothing recorded is its own case. Falling through to the mismatch below
	// would format "StateUnset is recorded", asserting a verdict that setNext
	// explicitly refuses to store — the sentinel is never a destination, so it
	// can never have been recorded.
	if a.next == StateUnset {
		return fmt.Errorf("%w: no destination is recorded; SetNext must record one before crossing", ErrIllegalTransition)
	}
	if a.next != to {
		return fmt.Errorf("%w: %s is recorded; cross cannot take %s instead", ErrIllegalTransition, a.next, to)
	}
	a.state = to
	a.activity = ActNone
	a.next = StateUnset
	return nil
}

// setActivity records what is executing. It is deliberately unvalidated
// against state: nothing branches on Activity, so a mismatch is a display bug
// rather than a correctness one, and a validation table here would be a second
// place to update whenever a stage moves.
func (a *Attempt) setActivity(x Activity) { a.activity = x }

// finish assigns the verdict and closes the attempt. Write-once: a second
// call is rejected rather than allowed to overwrite the first verdict,
// whatever it was.
//
// o must be a settled outcome AND a value AllOutcomes() actually declares.
// IsSettled alone is not enough — it reports true for any value other than
// OutcomePending, including one no const declares (Outcome(42)), because it
// is a zero-value check rather than a range check. Recording an unrecognized
// verdict would be a caller bug, not a legitimate outcome the machine
// produces, so it is rejected here rather than persisted.
//
// next is cleared alongside activity: without this, a cancel taken after
// Assessing had already recorded a verdict (e.g. next=Repairing) would leave
// a settled attempt still reporting a destination it will never move to —
// see TestAttempt_FinishClearsNext.
func (a *Attempt) finish(o Outcome, now time.Time) error {
	if !o.IsSettled() {
		return fmt.Errorf("%w: cannot finish an attempt with outcome %s", ErrInvalidOutcome, o)
	}
	if !slices.Contains(AllOutcomes(), o) {
		return fmt.Errorf("%w: cannot finish an attempt with unrecognized outcome %s", ErrInvalidOutcome, o)
	}
	// Write-once is checked before the boundary guard below: an attempt that
	// is both already settled AND crossed the boundary has its more
	// fundamental invariant violated first — a second finish call is wrong
	// regardless of which outcome it carries, while the boundary guard only
	// ever has something to say about OutcomeUnrecoverable specifically.
	// Checking the boundary first would report the crossing (from the
	// crossed guard, which stays true after a first finish settles the
	// attempt) for what is really a write-once violation.
	if a.outcome.IsSettled() {
		return fmt.Errorf("%w: %s, refusing to overwrite with %s", errOutcomeAlreadySet, a.outcome, o)
	}
	// OutcomeOK means "the job produced its files" (outcome.go), which cannot
	// be true before the boundary — production happens in Extracting and
	// Finalizing. Without this, Finish(OutcomeOK) settles an attempt in
	// Fetching, and BeginAttempt — which refuses a reopen only for an attempt
	// that crossed — then opens a SECOND attempt on a job already declared
	// complete. Guarding here rather than in BeginAttempt keeps one owner:
	// the door that assigns the verdict decides which verdicts are assignable.
	//
	// Both guards read a.crossed(), which is now IsProduction(a.state) rather
	// than a latch. finish no longer overwrites a.state, so the position
	// survives settling and answers the question directly.
	if o == OutcomeOK && !a.crossed() {
		return fmt.Errorf("%w: OutcomeOK claims the job produced its files, but this attempt never crossed into Production", ErrInvalidOutcome)
	}
	if o == OutcomeUnrecoverable && a.crossed() {
		return fmt.Errorf("%w: this attempt already crossed into Production", ErrUnrecoverableAfterBoundary)
	}
	a.activity = ActNone
	a.next = StateUnset
	a.outcome = o
	a.ended = now
	return nil
}
