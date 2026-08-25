package job

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrOutcomeAlreadySet is returned when a settled attempt is finished again.
// The write-once rule is enforced here rather than by convention, because a
// second assignment is exactly the mutation the design exists to prevent.
var ErrOutcomeAlreadySet = errors.New("job: attempt outcome already set")

// ErrFinishRequired is returned when transition is asked to reach Finished.
// finish is the only door into that state: it is the only mutator that
// assigns Outcome, so it must also be the only mutator that can leave an
// attempt in Finished — otherwise an attempt could reach Finished still
// carrying OutcomePending, and isOpen() would report an attempt open when
// nothing is ever going to close it.
var ErrFinishRequired = errors.New("job: transition cannot reach Finished; call finish instead")

// Attempt is one run of a job through the machine. The state machine lives
// here, not on Job: a job has a LIST of attempts, each carrying its own
// write-once Outcome, so a retry appends a verdict rather than revising one
// (spec §3.1).
//
// An attempt opens when a lease is first issued and no attempt is open, and
// closes when it reaches Finished. Pause and resume inside an attempt do not
// end it — the lease is surrendered and later re-taken, and the attempt
// persists across that.
//
// Every field is unexported and every mutator is package-private. Job is the
// only caller, and it holds its own lock across each of these; Attempt does
// no locking of its own.
type Attempt struct {
	state    State
	next     State
	reason   WaitReason
	activity Activity
	outcome  Outcome
	assessed bool
	started  time.Time
	ended    time.Time
}

// newAttempt opens an attempt in Fetching. There is no arm for opening in any
// other state: an attempt begins when a lease is issued, and a lease is what
// Fetching requires.
func newAttempt(now time.Time) Attempt {
	return Attempt{state: Fetching, started: now}
}

func (a *Attempt) isOpen() bool { return a.outcome == OutcomePending }

func (a *Attempt) view() StateView {
	return StateView{
		State:    a.state,
		Next:     a.next,
		Reason:   a.reason,
		Activity: a.activity,
		Outcome:  a.outcome,
		Assessed: a.assessed,
	}
}

// transition moves the attempt to `to`, rejecting any edge the machine does
// not contain. Activity is cleared, because it describes the state being left:
// carrying it forward would render a job as "repairing" while it extracts.
//
// transition cannot reach Finished — see ErrFinishRequired. It therefore
// takes no `now`: the only thing `now` was ever used for was timestamping an
// arrival at Finished, and finish already takes its own.
func (a *Attempt) transition(to State) error {
	if to == Finished {
		return ErrFinishRequired
	}
	if !CanTransition(a.state, to) {
		return illegalTransition(a.state, to)
	}
	a.state = to
	a.activity = ActNone
	a.next = Waiting
	a.reason = NoLease
	if to == Assessing {
		// Latches for the life of the attempt. ToSABnzbd reads it to tell a
		// first-pass download from a re-entry fetching recovery volumes,
		// which is what upstream's "Fetching" status means.
		a.assessed = true
	}
	return nil
}

// hold parks the attempt at a boundary. next is where it will resume, and is
// validated against the CURRENT state, not against Waiting — Waiting itself
// accepts a resume into any non-terminal state (it is the universal resume
// point in legalEdges), so checking CanTransition(Waiting, next) would let a
// hold defer an edge the pre-wait state could never have reached directly
// (e.g. Fetching parking with next=Repairing, which no direct Fetching edge
// permits). Validating against a.state instead makes a hold's destination
// exactly what the un-paused machine would have allowed.
func (a *Attempt) hold(next State, r WaitReason) error {
	if !CanTransition(a.state, Waiting) {
		return illegalTransition(a.state, Waiting)
	}
	if !CanTransition(a.state, next) {
		return illegalTransition(a.state, next)
	}
	a.state = Waiting
	a.next = next
	a.reason = r
	a.activity = ActNone
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
func (a *Attempt) finish(o Outcome, now time.Time) error {
	if !o.IsSettled() {
		return fmt.Errorf("job: cannot finish an attempt with outcome %s", o)
	}
	if !slices.Contains(AllOutcomes(), o) {
		return fmt.Errorf("job: cannot finish an attempt with unrecognized outcome %s", o)
	}
	if a.outcome.IsSettled() {
		return fmt.Errorf("%w: %s, refusing to overwrite with %s", ErrOutcomeAlreadySet, a.outcome, o)
	}
	a.state = Finished
	a.activity = ActNone
	a.outcome = o
	a.ended = now
	return nil
}
