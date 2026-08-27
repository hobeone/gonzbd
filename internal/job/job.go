package job

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// ErrNoOpenAttempt is returned by every door that needs an attempt already in
// flight, when the job has none — either because it has never run or because
// its last attempt is settled. Five doors return it — Transition, SetNext,
// SetActivity, Cross and Finish — and all five now return it from ONE check,
// in withOpenAttempt. Cross and Finish reach it through withOpenAttemptLease,
// a shape adapter that takes no lock and repeats no check, so there is a
// single answer to "must there be an open attempt?" rather than three copies
// that could drift. `git grep -n 'withOpenAttempt[(]' -- 'internal/job/*.go'
// ':!internal/job/*_test.go'` returns five lines: the three direct callers,
// the adapter's call, and the helper's own declaration. The bracket keeps this
// citation from matching its own text, and the pathspec drops the tests that
// call the helper directly.
// BeginAttempt is also a
// mutator but never returns this error: it is what opens an
// attempt in the first place. The caller's fix for this error is
// BeginAttempt, which is the only door into the machine.
var ErrNoOpenAttempt = errors.New("job: no open attempt")

// ErrBoundaryConsumed is returned by BeginAttempt when the job's most recent
// attempt crossed into Production. The guard reads a.crossed, deliberately
// NOT IsProduction(a.state) — this was true while finish overwrote a.state, so by
// the time BeginAttempt looks, IsProduction would read false on exactly the
// attempts this must refuse. D3 says crossing deletes
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
// because of what it renders as: ToSABnzbd never reads the intent field —
// `git grep -n 'v\.[I]ntent' internal/job/sabnzbd.go` exits 1 (the bracket
// stops the pattern matching this citation's own text; RenderView.Intent
// does appear in that file's comments, which a bare 'Intent' grep would also
// have matched). What reaches the user as that deleted status is the settled
// verdict OutcomeCancelled, mapped at sabnzbd.go:108 — its return statement
// is the only non-test site in this package that returns it (`git grep -n
// 'return constants.Status[D]eleted' -- 'internal/job/*.go'
// ':!internal/job/*_test.go'` returns exactly that one line).
//
// The latch is one-way because prior spec D8 makes a full redo a re-added
// NZB starting a NEW Job rather than a new attempt on this one. Clearing the
// latch would let a job the user deleted come back through a path that never
// re-asked them.
var ErrIntentLatched = errors.New("job: intent is latched; this job is cancelled")

// ErrInvalidIntent is returned when SetIntent is given a value that is not a
// declared Intent. TestAllIntents_Exhaustive keeps AllIntents() equal to
// intent.go's declarations, so this rejects exactly the undeclared values.
var ErrInvalidIntent = errors.New("job: invalid intent")

// ErrAlreadyLeased is returned by Grant when the job already holds a lease.
// Pool-A capacity is reserved across the whole correctness loop (prior spec
// §8.1), so a job re-entering Fetching from Assessing still holds the one it
// was given; a second grant would mean the Queue had issued capacity twice.
var ErrAlreadyLeased = errors.New("job: already holds a lease")

// Job owns its state. Every field is unexported. The lifecycle field —
// attempts — is guarded by mu, and there is no path to it that does not go
// through a method here. id, name and policy are not guarded: they are set
// once in New and never written again, so ID, Name and Policy read them
// without taking the lock.
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

	// lease is the admission token this job currently holds, or nil. Guarded
	// by mu. Granted by Grant; released by surrenderLocked, which is the sole
	// writer of nil into this field — `git grep -n 'j\.lease[ \t]*='
	// -- 'internal/job/*.go'` returns two hits, Grant writing l and
	// surrenderLocked writing nil. Cross and Finish both settle a job's need
	// for its lease and both yield it by calling surrenderLocked, not the
	// exported Surrender — see surrenderLocked's comment for why the
	// exported form would deadlock from either door.
	lease *Lease
}

// New builds a job that has never run. It has no attempt record, because
// nothing has happened to it yet.
func New(id, name string, p Policy) *Job {
	return &Job{id: id, name: name, policy: p}
}

// ID returns the job's identifier.
func (j *Job) ID() string { return j.id }

// Name returns the job's display name.
func (j *Job) Name() string { return j.name }

// Policy returns the job's retry/repair policy.
func (j *Job) Policy() Policy { return j.policy }

// State returns the current attempt's view, or a StateUnset view for a job that
// has never run. A job with no attempt is not AT a state — the old model
// answered Waiting{Next: Fetching}, which claimed a position the job had not
// reached. HasRun() distinguishes the two cases for a caller that needs to.
func (j *Job) State() StateView {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if a := j.currentLocked(); a != nil {
		return a.view()
	}
	return StateView{}
}

// HasRun reports whether this job has ever begun an attempt — len(attempts) >
// 0, nothing more. NOT whether it ever held a lease: D-I12 decoupled the two,
// so a job that has begun an attempt and is still waiting for pool A returns
// true here while j.lease is nil. Exact, where any predicate over bytes or
// durable runs would conflate "did not start" with "started and got
// nowhere".
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
// here rather than erroring — idempotence while an attempt is OPEN takes
// priority over the boundary refusal (open, not "holding a lease": Cross
// surrenders the lease at the boundary, so an open attempt past it holds a
// compute slot and no lease), and the boundary refusal only ever
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
	if a != nil && a.crossed() {
		return ErrBoundaryConsumed
	}
	j.attempts = append(j.attempts, newAttempt(now))
	return nil
}

// Transition moves the open attempt to the given state. It surfaces
// its errors unchanged — the settling door has its own
// door (Finish) and Transition is not it.
func (j *Job) Transition(to State) error {
	return j.withOpenAttempt(func(a *Attempt) error { return a.transition(to) })
}

// SetNext records that the open attempt's current state has finished its work,
// and where it continues to. See Attempt.setNext.
func (j *Job) SetNext(n State) error {
	return j.withOpenAttempt(func(a *Attempt) error { return a.setNext(n) })
}

// SetActivity records what the open attempt is currently executing.
func (j *Job) SetActivity(x Activity) error {
	return j.withOpenAttempt(func(a *Attempt) error { a.setActivity(x); return nil })
}

// Cross is the sole door across the irreversible boundary. It sets state,
// latches crossed, clears next and yields the lease in ONE call — there is no
// way to do one without the others.
//
// The lease may be nil: a job can legitimately reach the crossing holding
// none, having been paused at Assessing{next: Extracting} and resumed. It does
// not need one to cross, because crossing is where it would have given the
// lease up anyway. The Queue's reclaimer no-ops on nil rather than each caller
// testing for it.
//
// It surrenders under the same lock it mutates the attempt under, which is why
// it calls surrenderLocked rather than the exported Surrender — see
// surrenderLocked for why the latter would deadlock from inside a door.
func (j *Job) Cross(to State) (*Lease, error) {
	return j.withOpenAttemptLease(func(a *Attempt) (*Lease, error) {
		if err := a.cross(to); err != nil {
			return nil, err
		}
		return j.surrenderLocked(), nil
	})
}

// Finish assigns the verdict, closes the open attempt, and yields the lease if
// one is held.
//
// It yields the lease because every settling path ends the job's need for it:
// a pre-boundary failure, an Unrecoverable verdict from Assessing, and a
// cancel all settle without passing through Cross. An earlier design
// returned only an error, and leaked a pool-A slot on all three.
func (j *Job) Finish(o Outcome, now time.Time) (*Lease, error) {
	return j.withOpenAttemptLease(func(a *Attempt) (*Lease, error) {
		if err := a.finish(o, now); err != nil {
			return nil, err
		}
		return j.surrenderLocked(), nil
	})
}

// withOpenAttempt is the one preamble every door that mutates the current
// attempt goes through: take the write lock, resolve the open attempt or fail,
// apply. It is the only function in this file that answers "must there be an
// open attempt?", so the answer cannot drift between doors.
//
// Doors that must return the lease they yield use withOpenAttemptLease below,
// which is a shape adapter over this one — it takes no lock and repeats no
// check. Before that adapter existed, Cross and Finish copied this preamble
// inline, which made three independently-maintained copies of one question.
func (j *Job) withOpenAttempt(fn func(*Attempt) error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	a := j.currentLocked()
	if a == nil || !a.isOpen() {
		return ErrNoOpenAttempt
	}
	return fn(a)
}

// withOpenAttemptLease adapts withOpenAttempt for the doors that yield a lease
// on success. It changes the SHAPE of the callback's result and nothing else:
// no lock of its own, no second open-attempt check, so withOpenAttempt remains
// the sole owner of both.
//
// The callback runs with j.mu held, which is why a door that yields must call
// surrenderLocked and never the exported Surrender — see surrenderLocked.
//
// It does NOT enforce that an error path yields no lease — it returns whatever
// the callback returned, so a callback that returns (lease, err) hands back
// both. That is caller discipline, and a mutation proves it: making Cross's
// callback `return j.surrenderLocked(), err` does return a lease alongside the
// error. What the adapter gives is that the correct shape is also the natural
// one — `return nil, err` — and TestJob_FailedCrossAndFinishKeepTheLease is
// what actually enforces it, granting a lease and driving four error paths.
//
// The rule being protected: (*Lease, error) must never mean "failed, and here
// is a lease". The job is still in Correctness on that path and still needs
// its admission token.
func (j *Job) withOpenAttemptLease(fn func(*Attempt) (*Lease, error)) (*Lease, error) {
	var l *Lease
	err := j.withOpenAttempt(func(a *Attempt) error {
		var e error
		l, e = fn(a)
		return e
	})
	return l, err
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
	// Membership first, mirroring finish's slices.Contains(AllOutcomes(), o).
	// An out-of-range Intent stored here is not merely cosmetic: IsLatched()
	// is false for it, so the cancel latch never engages, and every gate that
	// compares against IntentPause or IntentCancel reads the job as un-gated
	// while telemetry publishes "Intent(99)".
	if !slices.Contains(AllIntents(), i) {
		return fmt.Errorf("%w: %s is not a declared intent", ErrInvalidIntent, i)
	}
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
// Cross and Finish, the two doors that end a job's need for a lease, run their
// bodies inside withOpenAttempt's callback, which holds j.mu — so a call to
// the exported Surrender() from either would take j.mu a second time and
// deadlock the job permanently, with no error and no timeout. Both call this
// helper instead, which needs no lock of its own.
//
// Three callers of surrenderLocked as of this commit: Surrender (above),
// Cross and Finish (job.go) — `git grep -n 'surrender[L]ocked()' --
// 'internal/job/*.go' ':!internal/job/*_test.go'` returns four lines, one
// being this method's own definition.
func (j *Job) surrenderLocked() *Lease {
	l := j.lease
	j.lease = nil
	return l
}
