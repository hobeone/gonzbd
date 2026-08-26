package job

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNoOpenAttempt is returned by the four mutators withOpenAttempt wraps —
// Transition, Hold, SetActivity and Finish — when the job has no attempt in
// flight, either because it has never run or because its last attempt is
// settled. BeginAttempt and SetWaitReason are also mutators but do not go
// through withOpenAttempt and so never return this error: BeginAttempt is
// what opens an attempt in the first place, and SetWaitReason's never-run
// case (see its own doc comment) exists specifically to write j.pending
// while no attempt is open. The caller's fix for this error is BeginAttempt,
// which is the only door into the machine.
var ErrNoOpenAttempt = errors.New("job: no open attempt")

// ErrBoundaryConsumed is returned by BeginAttempt when the job's most recent
// attempt crossed into Production (IsProduction). D3 says crossing deletes
// archives, moves files, and consumes the inputs a later attempt would need
// — "not crossing keeps the job retryable" — so a fresh attempt on THIS job
// is no longer a legal way to retry it once one has crossed. The Attempt
// state machine's one-way boundary (TestBoundaryIsOneWay) holds only within
// a single Attempt; Job holds a LIST of them, and appending a new one was
// how a crossed job could walk back into Correctness before this guard
// existed. The user-facing path for a full redo is D8's re-added NZB, which
// starts a new Job, not a new Attempt on this one.
var ErrBoundaryConsumed = errors.New("job: cannot begin a new attempt; a prior attempt crossed the Correctness/Production boundary")

// ErrIntentLatched is returned by SetIntent when the job has already been
// cancelled. Cancel is final for a Job because of where it leads, not
// because of what it renders as: the intent is not consulted by rendering
// at all (`git grep -n 'Intent' internal/job/sabnzbd.go` exits 1). What
// reaches the user as StatusDeleted is the settled verdict OutcomeCancelled,
// mapped at sabnzbd.go:100 — the sole arm returning it.
//
// The latch is one-way because prior spec D8 makes a full redo a re-added
// NZB starting a NEW Job rather than a new attempt on this one. Clearing the
// latch would let a job the user deleted come back through a path that never
// re-asked them.
var ErrIntentLatched = errors.New("job: intent is latched; this job is cancelled")

// ErrAlreadyLeased is returned by Grant when the job already holds a lease.
// Pool-A capacity is reserved across the whole correctness loop (prior spec
// §8.1), so a job re-entering Fetching from Assessing still holds the one it
// was given; a second grant would mean the Queue had issued capacity twice.
var ErrAlreadyLeased = errors.New("job: already holds a lease")

// Job owns its state. Every field is unexported. The lifecycle fields —
// attempts and pending — are guarded by mu, and there is no path to either
// that does not go through a method here. id, name and policy are not
// guarded: they are set once in New and never written again, so ID, Name and
// Policy read them without taking the lock.
//
// What is established now: a Job method never calls any other repository
// package's method, because this package imports nothing from the rest of
// the daemon except internal/constants, imported only by sabnzbd.go among
// this package's non-test sources — see doc.go and
// TestOnlyOneNonTestFileImportsConstants (sabnzbd_test.go), which is what
// checks that; `go list -deps` cannot, since it does not see test files, and
// sabnzbd_test.go itself imports internal/constants. In particular Job
// cannot call Queue, because Job cannot see Queue.
//
// What is intent for a later plan, not yet built or enforced: a Queue type
// that always locks Queue.mu before calling into Job.mu, giving the system a
// single lock order. Nothing in this repository defines Queue yet, so that
// half of the ordering claim has no enforcement point until it does.
//
// Job does no I/O. It exposes State() and the attempt accessors. The later
// plan's design intent is a Checkpointer that reads those and writes the
// database; no such type exists in this repository today
// (`git grep -n 'type[ ]Checkpointer'`, run from the repository root,
// returns nothing — the bracketed space is so this citation, quoted
// verbatim, does not match its own quoted text).
type Job struct {
	mu sync.RWMutex

	id     string
	name   string
	policy Policy

	// attempts is the machine. The current attempt is the last element, and
	// an empty slice means the job has never run — which is what HasRun
	// reports and what makes "never started" exact rather than a predicate
	// over byte counters (D1).
	//
	// Deliberately unbounded (D7). The growth case is one job an automation
	// tool retries on a schedule; each Attempt is a handful of words, and the
	// two remedies if it ever bites are a cap here or a sweep alongside
	// history retention. Not worth a policy before there is evidence.
	attempts []Attempt

	// intent is what a person has asked of this job. Guarded by mu. Sole
	// writer: SetIntent — enforced by
	// TestIntentWrites_MatchTheEnumerationStatedInProse (task 7), not by this
	// comment.
	intent Intent

	// pending is the wait reason a job carries before its first attempt
	// exists. A never-run job has no Attempt to hold a reason on — Attempt's
	// own reason field only exists once an attempt has been opened — so a
	// pause arriving before BeginAttempt has nowhere to record itself
	// without this field. Exactly two writes — New's initializer and
	// SetWaitReason's assignment — confirmed by `git grep -n 'pending[:][
	// ]NoLease\|[.]pending[ ]=' -- internal/job/*.go`, which returns those
	// two lines and nothing else. The bracketed single-character classes
	// around the colon, the space and the dot are there so this citation,
	// quoted verbatim inside this comment, does not match its own quoted
	// text as a substring the way an earlier draft of this comment did.
	pending WaitReason

	// lease is the admission token this job currently holds, or nil. Guarded
	// by mu. Granted by Grant; released by surrenderLocked, which is the sole
	// writer of nil into this field. At this commit Surrender is its only
	// caller — Cross does not yet exist in this package (it is task 6) and
	// Finish does not yet touch lease at all (its signature changes to yield
	// one, also task 6); both are wired through surrenderLocked then.
	lease *Lease
}

// New builds a job that has never run. It has no attempt record, because
// nothing has happened to it yet. pending starts at NoLease, matching the
// constant State() has always reported for a never-run job — a fresh Job's
// behavior is unchanged unless SetWaitReason is called.
func New(id, name string, p Policy) *Job {
	return &Job{id: id, name: name, policy: p, pending: NoLease}
}

// ID returns the job's identifier.
func (j *Job) ID() string { return j.id }

// Name returns the job's display name.
func (j *Job) Name() string { return j.name }

// Policy returns the job's retry/repair policy.
func (j *Job) Policy() Policy { return j.policy }

// State returns the current attempt's view. For a job that has never run the
// answer is a constant shape rather than a special case: it is waiting for a
// lease, which is exactly what is true. Reason comes from j.pending rather
// than a hardcoded NoLease, so a pause recorded via SetWaitReason before the
// job's first attempt (e.g. UserPaused) is visible here — New initializes
// pending to NoLease, so this is unchanged from before unless SetWaitReason
// has been called.
func (j *Job) State() StateView {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if a := j.currentLocked(); a != nil {
		return a.view()
	}
	return StateView{State: Waiting, Next: Fetching, Reason: j.pending}
}

// HasRun reports whether this job has ever held a lease. Exact, where any
// predicate over bytes or durable runs would conflate "did not start" with
// "started and got nowhere".
func (j *Job) HasRun() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.attempts) > 0
}

// Attempts returns how many times this job has run.
func (j *Job) Attempts() int {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.attempts)
}

// AttemptAt returns a view of the i'th attempt. Panics on an out-of-range
// index, matching slice semantics — callers bound i with Attempts().
func (j *Job) AttemptAt(i int) StateView {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.attempts[i].view()
}

// BeginAttempt opens an attempt if none is open, and is a no-op if one
// already is. That is the rule that stops a pause/resume cycle — which
// surrenders and later re-takes a lease — from being counted as a retry.
//
// It refuses (ErrBoundaryConsumed) instead of appending when the most
// recent attempt crossed into Production, even though that attempt is
// closed. The open check runs first and returns nil before the crossed
// check is even reached, so an open attempt that has crossed still no-ops
// here rather than erroring — idempotence while a lease is held takes
// priority over the boundary refusal, and the boundary refusal only ever
// applies to starting a NEW attempt.
//
// Checking only the most recent attempt, rather than every element of
// j.attempts, is enough: this method is the only place attempts is written
// to. TestAttemptsWrites_MatchTheEnumerationStatedInProse
// (attempts_writer_enumeration_test.go) parses the package's non-test
// sources and enforces that as a population, not a one-time grep frozen in
// prose, so once this guard exists, no attempt can ever be appended after
// one that crossed — an attempt that crossed is therefore always last, and
// checking the last one is checking all of them.
func (j *Job) BeginAttempt(now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	a := j.currentLocked()
	if a != nil && a.isOpen() {
		return nil
	}
	if a != nil && a.crossed {
		return ErrBoundaryConsumed
	}
	j.attempts = append(j.attempts, newAttempt(now))
	return nil
}

// Transition moves the open attempt to the given state. It surfaces
// ErrHoldRequired and ErrFinishRequired unchanged when to is Waiting or
// Finished respectively — those states each have their own door (Hold,
// Finish) and Transition is not one of them.
func (j *Job) Transition(to State) error {
	return j.withOpenAttempt(func(a *Attempt) error { return a.transition(to) })
}

// Hold parks the open attempt at a boundary, to resume at next for reason r.
func (j *Job) Hold(next State, r WaitReason) error {
	return j.withOpenAttempt(func(a *Attempt) error { return a.hold(next, r) })
}

// SetActivity records what the open attempt is currently executing.
func (j *Job) SetActivity(x Activity) error {
	return j.withOpenAttempt(func(a *Attempt) error { a.setActivity(x); return nil })
}

// Finish assigns the verdict and closes the open attempt.
func (j *Job) Finish(o Outcome, now time.Time) error {
	return j.withOpenAttempt(func(a *Attempt) error { return a.finish(o, now) })
}

// SetWaitReason records why the job is waiting, covering the two shapes
// nothing else in this package can update in place:
//
//   - No attempt exists yet (HasRun() == false): there is no Attempt to
//     carry a reason, so this writes j.pending directly.
//   - An attempt is open and parked (State() == Waiting): Hold already
//     decided the destination (next), and its own doc comment explains why
//     that must not be re-declared by a second Hold call — but the REASON
//     legitimately changes while parked (NoComputeSlot -> GlobalPause, when
//     a global pause arrives while a job already waits for a compute slot).
//     This delegates to setReason, which writes only a.reason.
//
// Deliberately NOT routed through withOpenAttempt: that helper's !a.isOpen()
// check would reject the never-run case outright — no attempt is ever open
// before the first BeginAttempt — which is half of what this method exists
// to cover. An attempt that is open but not Waiting (mid-work) still fails,
// via setReason's own ErrNotWaiting: SetWaitReason cannot alter a.next, only
// a.reason, so there is nothing for it to do while an attempt is actively
// running.
func (j *Job) SetWaitReason(r WaitReason) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	a := j.currentLocked()
	if a == nil {
		j.pending = r
		return nil
	}
	return a.setReason(r)
}

// withOpenAttempt is the single door every mutator goes through: take the
// write lock, resolve the open attempt or fail, apply. One door rather than
// four copies of the same preamble, so "must there be an open attempt?" has
// one answer that cannot drift between mutators.
func (j *Job) withOpenAttempt(fn func(*Attempt) error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	a := j.currentLocked()
	if a == nil || !a.isOpen() {
		return ErrNoOpenAttempt
	}
	return fn(a)
}

// currentLocked returns a pointer to the last attempt, or nil if there are
// none. Must hold mu.
func (j *Job) currentLocked() *Attempt {
	if len(j.attempts) == 0 {
		return nil
	}
	return &j.attempts[len(j.attempts)-1]
}

// Intent reports what has been asked of this job.
func (j *Job) Intent() Intent {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.intent
}

// SetIntent records what is being asked of this job. Legal in every state,
// including once the current attempt is settled: a settled job may be retried,
// and the intent it carries governs what happens when it is.
//
// Refuses only when the job is already cancelled, and only for a DIFFERENT
// intent — re-asserting cancel is an idempotent no-op rather than an error,
// since a retrying caller repeating itself is not a mistake to report.
func (j *Job) SetIntent(i Intent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.intent.IsLatched() && i != j.intent {
		return fmt.Errorf("%w: cannot replace %s with %s", ErrIntentLatched, j.intent, i)
	}
	j.intent = i
	return nil
}

// HoldsLease reports whether this job currently holds its admission token.
// This is half of what makes a job "running" — see the design's §3.4.
func (j *Job) HoldsLease() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.lease != nil
}

// Grant hands this job an admission token. Refuses nil, which would be
// indistinguishable from holding none, and refuses a second lease.
func (j *Job) Grant(l *Lease) error {
	if l == nil {
		return fmt.Errorf("job: Grant(nil): a nil lease is indistinguishable from holding none")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.lease != nil {
		return ErrAlreadyLeased
	}
	j.lease = l
	return nil
}

// Surrender yields the lease, or nil if none is held. Callers that already
// hold j.mu must use surrenderLocked instead — see its comment.
func (j *Job) Surrender() *Lease {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.surrenderLocked()
}

// surrenderLocked is the sole releaser of j.lease. Must hold mu.
//
// It exists because j.mu is a sync.RWMutex and Go mutexes are NOT reentrant.
// The doors that will end a job's need for a lease — Cross and Finish — will
// reach the attempt through withOpenAttempt, which takes j.mu.Lock() and
// holds it across its callback. A door calling the exported Surrender() from
// there would take j.mu a second time and deadlock the job permanently, with
// no error and no timeout. Routing releases through this helper keeps one
// releaser without reacquiring anything.
//
// Not yet true at this commit: Cross does not exist in this package (task 6
// adds it), and Finish does not call this method — Finish's signature
// changes to yield the lease, also in task 6. Today Surrender is this
// method's only caller.
func (j *Job) surrenderLocked() *Lease {
	l := j.lease
	j.lease = nil
	return l
}
