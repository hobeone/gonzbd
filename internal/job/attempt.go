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
// Attempt.finish directly (e.g. this package's own tests). Job.Finish goes
// through withOpenAttempt, whose !a.isOpen() check fires first on a settled
// attempt and returns ErrNoOpenAttempt instead — a strict superset of this
// check, since isOpen() is itself defined as outcome == OutcomePending. No
// external caller can ever reach this branch through the public API, so
// exporting it would be a dead sentinel — see withOpenAttempt in job.go,
// which deliberately does not surface this error.
var errOutcomeAlreadySet = errors.New("job: attempt outcome already set")

// ErrUnrecoverableAfterBoundary is returned when finish is asked to record
// OutcomeUnrecoverable for an attempt that has crossed into Production
// (a.crossed), whether or not it is still there — a held attempt reads back
// as Waiting, not as the Production state it crossed at, so the guard tracks
// the latch rather than the transient state. D3 defines OutcomeUnrecoverable
// as "the job never crossed the boundary" specifically so its files stay in
// the working directory and the job stays retryable — a verdict finish must
// not let contradict where the attempt actually crossed. This is a sentinel
// rather than a plain fmt.Errorf: a caller that reaches Production and then
// gets an Unrecoverable verdict from downstream (e.g. par2 misclassifying a
// Production-stage fault) has a caller bug to fix, not a job to fail, and
// distinguishing that case with errors.Is is the whole reason to name it.
var ErrUnrecoverableAfterBoundary = errors.New("job: cannot record Unrecoverable for an attempt past the Correctness/Production boundary")

// ErrFinishRequired is returned when transition is asked to reach Finished,
// or hold is asked to resume into it. finish is the only door into that
// state: TestOutcomeWrites_MatchTheEnumerationStatedInProse enumerates every
// site that sets the unexported outcome field — a plain `=` assignment and a
// `outcome: x` composite-literal key alike — across this package's non-test
// sources, and asserts finish is the only one, so this sentence is checked
// by a test rather than trusted as prose — otherwise an attempt could reach
// Finished still carrying OutcomePending, and isOpen() would report an
// attempt open when nothing is ever going to close it.
var ErrFinishRequired = errors.New("job: transition cannot reach Finished; call finish instead")

// ErrHoldRequired is returned when transition is asked to reach Waiting.
// hold is the only door into that state: transition has neither a
// destination-with-a-reason argument nor a next field to fill in on its own,
// so entering Waiting through transition instead of hold left an attempt
// with next equal to Waiting itself — unable to resume (transition's a.next
// check accepts only next as a destination, and Waiting is not a work state)
// and unable to be re-parked (hold refuses whenever the attempt is already
// Waiting). Only finish could ever move it again. Refusing here is the fix,
// not defaulting to some next: you cannot pause without saying where you are
// going and why, and transition is never given either.
var ErrHoldRequired = errors.New("job: transition cannot reach Waiting; call hold instead")

// ErrInvalidOutcome is returned when finish is asked to record a verdict
// that is not a legitimate outcome the machine produces — either
// OutcomePending (not a verdict at all) or a value AllOutcomes() does not
// declare. Exported, unlike errOutcomeAlreadySet: an external caller of
// Job.Finish can pass an arbitrary Outcome and observe this branch directly
// (withOpenAttempt's !a.isOpen() guard does not shadow it the way it shadows
// the write-once case), so a caller distinguishing "bad argument" from other
// finish failures needs a sentinel to match against.
var ErrInvalidOutcome = errors.New("job: invalid outcome")

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
	// crossed latches once this attempt actually arrives in Production
	// (IsProduction(state)) via transition — not merely holds toward it:
	// hold(next: Extracting) sets a.state to Waiting, not to Extracting, so
	// it never runs the line that sets this. Both finish's a.state =
	// Finished and hold's a.state = Waiting erase the state the attempt
	// crossed at, which is why this cannot be read back from a.state and
	// has to be latched when it happens, the same reason `assessed` exists.
	// Two field reads (this comment and other comments mentioning it are
	// not reads): finish, below in this file, guards OutcomeUnrecoverable
	// past the boundary even across a hold; and job.go's BeginAttempt
	// refuses to open a fresh attempt once a prior one crossed, because D3
	// says crossing consumes what a retry would need (spec
	// docs/superpowers/specs/2026-08-25-job-lifecycle-design.md, D3). Not on
	// StateView — nothing outside this package needs it.
	crossed bool
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
// Each non-work state has exactly one door, and this is the whole contract:
//
//	hold        -> Waiting
//	finish      -> Finished
//	transition  -> every other (work) state
//
// transition refuses both Waiting (ErrHoldRequired) and Finished
// (ErrFinishRequired) as destinations, because pausing and finishing each
// need something transition does not carry: hold takes a destination AND a
// reason, finish takes a verdict, and transition has neither. You cannot
// pause without saying where you are going and why. Entering Waiting through
// transition instead of hold was reachable before this check existed, and it
// stranded the attempt: transition's own a.next check (below) then accepted
// nothing but Waiting itself as a resume target, and hold refuses to re-park
// an attempt that is already Waiting — only finish could still move it.
//
// From Waiting, the only legal `to` is a.next. The destination was decided
// when hold was taken, and CanTransition(Waiting, to) alone cannot see that:
// Waiting's legalEdges accept resuming into any non-terminal state, which is
// what let Production and Correctness compose two individually legal edges —
// a pause into Waiting, then an unconstrained resume out of it — into the
// single edge legalEdges forbids directly (Extracting -> Waiting -> Fetching
// was reachable before this check existed). Requiring to == a.next is what
// keeps Waiting a non-branching node: it carries the one decision Assessing
// already made, rather than making a second decision of its own.
func (a *Attempt) transition(to State) error {
	if to == Finished {
		return ErrFinishRequired
	}
	if to == Waiting {
		return ErrHoldRequired
	}
	if a.state == Waiting && to != a.next {
		return illegalTransition(a.state, to)
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
	if IsProduction(to) {
		// See the crossed field's doc comment: this is the one place an
		// attempt actually arrives in Production, as opposed to merely
		// holding toward it.
		a.crossed = true
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
//
// hold refuses when the attempt is already Waiting: the destination was
// decided by the first hold that parked it there, and a second hold would
// let that destination be re-declared instead of merely resumed from
// (Extracting -> hold(Finalizing) -> hold(Fetching) -> transition(Fetching)
// was reachable before this check existed, even with transition's a.next
// check in place). The reason a job is waiting may legitimately change while
// it waits (NoComputeSlot -> GlobalPause); the destination may not, and this
// method only ever assigns both together — a reason-only update is deferred
// to the next plan's Queue work, not hypothetical: this package cannot
// express NoComputeSlot -> GlobalPause today (hold's own refusal above is
// what blocks it), and a global-pause arriving while a job waits for a
// compute slot needs to be recordable. The fix is a reason-only mutator with
// its own owner, never a second meaning for hold — see
// docs/superpowers/plans/2026-08-25-job-lifecycle-core.md, "What this plan
// does not deliver, and what comes next".
//
// hold also refuses next == Finished (finish is the sole door, see
// ErrFinishRequired: hold(Assessing) followed by transition(Repairing) must
// not be able to reach Finished by a route finish never validated) and
// next == Waiting (self-referential — there is nothing to resume into).
func (a *Attempt) hold(next State, r WaitReason) error {
	if a.state == Waiting {
		return fmt.Errorf("%w: already waiting; hold cannot re-declare the destination once held", ErrIllegalTransition)
	}
	if next == Finished {
		return ErrFinishRequired
	}
	if next == Waiting {
		return fmt.Errorf("%w: cannot hold with next=Waiting", ErrIllegalTransition)
	}
	// No separate CanTransition(a.state, Waiting) check: it can only ever
	// fire when a.state == Finished (every non-terminal state has a pause
	// edge to Waiting), and next == Finished is already excluded above, so
	// CanTransition(Finished, next) below is false for every next that
	// reaches this line — the same rejection, from the same cause, one line
	// down. A dedicated check here would never independently reject
	// anything; TestAttempt_HoldRejectsAfterFinish pins that the remaining
	// check alone still refuses hold once an attempt is Finished.
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
//
// next and reason are cleared alongside activity: StateView documents all
// three as meaningful only when State is Waiting, and without this a cancel
// taken while paused would leave a Finished attempt reporting the stale hold
// it was cancelled out of (e.g. Next=Assessing, Reason=UserPaused).
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
	// Checking the boundary first would report "state is Finished" (from the
	// crossed guard, since a first finish to Finished sets a.state =
	// Finished) for what is really a write-once violation.
	if a.outcome.IsSettled() {
		return fmt.Errorf("%w: %s, refusing to overwrite with %s", errOutcomeAlreadySet, a.outcome, o)
	}
	// Guard on a.crossed, not IsProduction(a.state): hold sets a.state to
	// Waiting, so a state-based check would miss an attempt that crossed
	// into Production and was then held there — Unrecoverable must stay
	// refused across a hold, since D3's "never crossed the boundary" verdict
	// would otherwise contradict BeginAttempt's separate refusal to open a
	// fresh attempt once crossed (ErrBoundaryConsumed, job.go). a.crossed
	// alone is equivalent to `a.crossed || IsProduction(a.state)`: transition
	// is the only place a.state is ever set to a Production state (newAttempt
	// starts at Fetching; hold only ever writes Waiting), and transition sets
	// a.crossed = true in the same call, with no early return between the
	// two writes — see the crossed field's doc comment.
	if o == OutcomeUnrecoverable && a.crossed {
		return fmt.Errorf("%w: state is %s", ErrUnrecoverableAfterBoundary, a.state)
	}
	a.state = Finished
	a.activity = ActNone
	a.next = Waiting
	a.reason = NoLease
	a.outcome = o
	a.ended = now
	return nil
}
