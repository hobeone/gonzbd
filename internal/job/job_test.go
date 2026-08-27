package job

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func newTestJob(t *testing.T) *Job {
	t.Helper()
	return New("abc123", "Test.Job", PolicyFromPP(3))
}

// TestJob_Accessors pins the three plain getters against the values New was
// given — the only thing there is to assert about them.
func TestJob_Accessors(t *testing.T) {
	wantPolicy := PolicyFromPP(3)
	j := New("abc123", "Test.Job", wantPolicy)
	if got := j.ID(); got != "abc123" {
		t.Errorf("ID() = %q, want %q", got, "abc123")
	}
	if got := j.Name(); got != "Test.Job" {
		t.Errorf("Name() = %q, want %q", got, "Test.Job")
	}
	if got := j.Policy(); got != wantPolicy {
		t.Errorf("Policy() = %+v, want %+v", got, wantPolicy)
	}
}

// TestJob_CurrentLockedAndWithOpenAttempt calls the two unexported helpers
// underlying every mutator directly, rather than only through the exported
// methods that already exercise them: currentLocked's nil-on-empty branch and
// withOpenAttempt's ErrNoOpenAttempt branch are otherwise only reached
// indirectly.
func TestJob_CurrentLockedAndWithOpenAttempt(t *testing.T) {
	j := newTestJob(t)

	j.mu.RLock()
	if got := j.currentLocked(); got != nil {
		j.mu.RUnlock()
		t.Fatalf("currentLocked() on a job with no attempts = %v, want nil", got)
	}
	j.mu.RUnlock()

	if err := j.withOpenAttempt(func(*Attempt) error { return nil }); !errors.Is(err, ErrNoOpenAttempt) {
		t.Errorf("withOpenAttempt on a job with no open attempt = %v, want ErrNoOpenAttempt", err)
	}

	mustBegin(t, j)
	j.mu.RLock()
	if got := j.currentLocked(); got == nil {
		j.mu.RUnlock()
		t.Fatal("currentLocked() after BeginAttempt = nil, want the open attempt")
	}
	j.mu.RUnlock()

	// The comparison target is fetched INSIDE the withOpenAttempt callback,
	// under the lock withOpenAttempt already holds, rather than captured
	// once via an earlier RLock/RUnlock and compared later — a pointer read
	// outside the lock and compared after release is not something the lock
	// protects, even though j.attempts (a slice) is never reallocated by any
	// mutator in this specific sequence. currentLocked itself takes no lock
	// (must hold mu, per its doc comment); calling it here is safe because
	// withOpenAttempt's Lock is already held by this goroutine.
	called := false
	if err := j.withOpenAttempt(func(a *Attempt) error {
		called = true
		if got := j.currentLocked(); a != got {
			t.Errorf("withOpenAttempt passed %p, want the same attempt currentLocked returned (%p)", a, got)
		}
		return nil
	}); err != nil {
		t.Errorf("withOpenAttempt on a job with an open attempt: %v", err)
	}
	if !called {
		t.Error("withOpenAttempt did not invoke fn")
	}
}

func TestJob_BeginAttemptOpensOne(t *testing.T) {
	j := newTestJob(t)
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if !j.HasRun() || j.Attempts() != 1 {
		t.Errorf("HasRun()=%v Attempts()=%d, want true and 1", j.HasRun(), j.Attempts())
	}
	if got := j.State().State; got != Fetching {
		t.Errorf("State = %v, want Fetching", got)
	}
}

// TestJob_BeginAttemptIsIdempotentWhileOneIsOpen pins the rule that a second
// BeginAttempt on a job whose current attempt is still open (not settled) is
// a no-op rather than a fresh attempt — a job that is not running is still at
// the state it last occupied (design §3.2), so a lease re-issued after a
// pool-B wait must not be counted as a retry.
func TestJob_BeginAttemptIsIdempotentWhileOneIsOpen(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if err := j.BeginAttempt(testClock().Add(time.Hour)); err != nil {
		t.Fatalf("second BeginAttempt: %v", err)
	}
	if got := j.Attempts(); got != 1 {
		t.Errorf("Attempts() = %d after a second BeginAttempt on an open attempt, want 1; "+
			"an attempt closes only when finish assigns its verdict", got)
	}
}

// TestJob_RetryAppendsAnAttempt is D2's core property: the previous verdict
// survives, and the new attempt starts pending.
func TestJob_RetryAppendsAnAttempt(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if _, err := j.Finish(OutcomeUnrecoverable, testClock()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := j.State().Outcome; got != OutcomeUnrecoverable {
		t.Fatalf("Outcome = %v, want Unrecoverable", got)
	}

	if err := j.BeginAttempt(testClock().Add(time.Hour)); err != nil {
		t.Fatalf("retry BeginAttempt: %v", err)
	}
	if got := j.Attempts(); got != 2 {
		t.Errorf("Attempts() = %d, want 2", got)
	}
	v := j.State()
	if v.State != Fetching || v.Outcome != OutcomePending {
		t.Errorf("State() = %+v; want a fresh Fetching attempt with a pending outcome", v)
	}
	if got := j.AttemptAt(0).Outcome; got != OutcomeUnrecoverable {
		t.Errorf("first attempt's Outcome = %v, want Unrecoverable preserved", got)
	}
}

func TestJob_MutatorsRequireAnOpenAttempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Job) error
	}{
		{"Transition", func(j *Job) error { return j.Transition(Assessing) }},
		{"SetNext", func(j *Job) error { return j.SetNext(Assessing) }},
		{"SetActivity", func(j *Job) error { return j.SetActivity(ActUnpack) }},
		{"Finish", func(j *Job) error { _, err := j.Finish(OutcomeFailed, testClock()); return err }},
		// Cross belongs here even though it can never succeed from a never-run
		// job for a second reason (it is legal only from Assessing): the
		// open-attempt check runs FIRST, so this pins which of the two errors
		// the door owes. All five now share one check in withOpenAttempt, and
		// that is what this table demonstrates rather than assumes — dropping
		// !a.isOpen() from that single helper fails all five subtests of
		// TestJob_FinishedJobHasNoOpenAttempt at once.
		{"Cross", func(j *Job) error { _, err := j.Cross(Extracting); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := newTestJob(t)
			if err := tc.call(j); !errors.Is(err, ErrNoOpenAttempt) {
				t.Errorf("%s on a never-run job = %v, want ErrNoOpenAttempt", tc.name, err)
			}
		})
	}
}

// TestJob_FinishedJobHasNoOpenAttempt is the settled-attempt half of
// TestJob_MutatorsRequireAnOpenAttempt's never-run half. Both halves are
// needed and neither implies the other: the never-run case reaches the guard
// through a == nil and the settled case through !a.isOpen(), so a door that
// dropped the second half would still pass the first. Cross is the door that
// proves it — with !a.isOpen() removed from Cross and a == nil kept, the whole
// package passed, because Cross on a settled attempt falls through to a.cross,
// which finds no edge out of Finalizing and returns ErrIllegalTransition
// instead. The wrong error, and no test to say so.
func TestJob_FinishedJobHasNoOpenAttempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Job) error
	}{
		{"Transition", func(j *Job) error { return j.Transition(Fetching) }},
		{"SetNext", func(j *Job) error { return j.SetNext(Assessing) }},
		{"SetActivity", func(j *Job) error { return j.SetActivity(ActUnpack) }},
		{"Finish", func(j *Job) error { _, err := j.Finish(OutcomeFailed, testClock()); return err }},
		{"Cross", func(j *Job) error { _, err := j.Cross(Extracting); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := newTestJob(t)
			mustBegin(t, j)
			if _, err := j.Finish(OutcomeFailed, testClock()); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if err := tc.call(j); !errors.Is(err, ErrNoOpenAttempt) {
				t.Errorf("%s after Finish = %v, want ErrNoOpenAttempt", tc.name, err)
			}
		})
	}
}

// TestJob_ConcurrentReadsAndWrites is the race-detector pin on Job owning its
// own lock. It asserts no outcome beyond "this does not race" — correctness
// of the transitions is covered above.
func TestJob_ConcurrentReadsAndWrites(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				_ = j.State()
				_ = j.HasRun()
				_ = j.Attempts()
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 200 {
				_ = j.SetActivity(ActPar2Verify)
				_ = j.SetActivity(ActNone)
			}
		})
	}
	wg.Wait()
}

// TestJob_BeginAttemptRefusesAfterCrossing pins the finding: an attempt that
// reached Production (IsProduction) and then finished must not let a later
// BeginAttempt walk the job back into Correctness. The state machine's
// one-way boundary is enforced inside a single Attempt (TestBoundaryIsOneWay),
// but Job holds a LIST of attempts, and appending a fresh one was previously
// unguarded — the exact probe this test encodes: an attempt that reached
// Extracting, then settled with OutcomeOK, let BeginAttempt open a second
// attempt back at Fetching with err == nil.
func TestJob_BeginAttemptRefusesAfterCrossing(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if err := j.Transition(Assessing); err != nil {
		t.Fatalf("Transition(Assessing): %v", err)
	}
	if err := j.SetNext(Extracting); err != nil {
		t.Fatalf("SetNext(Extracting): %v", err)
	}
	if _, err := j.Cross(Extracting); err != nil {
		t.Fatalf("Cross(Extracting): %v", err)
	}
	if err := j.Transition(Finalizing); err != nil {
		t.Fatalf("Transition(Finalizing): %v", err)
	}
	if _, err := j.Finish(OutcomeFailed, testClock()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := j.Attempts(); got != 1 {
		t.Fatalf("Attempts() before retry = %d, want 1", got)
	}

	err := j.BeginAttempt(testClock().Add(time.Hour))
	if !errors.Is(err, ErrBoundaryConsumed) {
		t.Errorf("BeginAttempt after crossing = %v, want ErrBoundaryConsumed", err)
	}
	if got := j.Attempts(); got != 1 {
		t.Errorf("Attempts() after refused retry = %d, want 1 (no attempt appended)", got)
	}
	// Finalizing, not some terminal value: the refused retry left the settled
	// attempt exactly where it was, which is also what makes crossed()
	// answerable without a latch.
	if got := j.State().State; got != Finalizing {
		t.Errorf("State() after refused retry = %v, want Finalizing (unchanged)", got)
	}
}

// TestJob_BeginAttemptStillIdempotentWhenOpenAttemptHasCrossed pins the
// ordering: the open-attempt no-op check must run before the crossed check,
// so a still-open attempt that already crossed does not turn a routine
// pause/resume BeginAttempt call into an error.
func TestJob_BeginAttemptStillIdempotentWhenOpenAttemptHasCrossed(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if err := j.Transition(Assessing); err != nil {
		t.Fatalf("Transition(Assessing): %v", err)
	}
	if err := j.SetNext(Extracting); err != nil {
		t.Fatalf("SetNext(Extracting): %v", err)
	}
	if _, err := j.Cross(Extracting); err != nil {
		t.Fatalf("Cross(Extracting): %v", err)
	}
	// Extracting is Production, but this attempt is still open (unfinished).
	if err := j.BeginAttempt(testClock().Add(time.Hour)); err != nil {
		t.Errorf("BeginAttempt on an open crossed attempt = %v, want nil (no-op)", err)
	}
	if got := j.Attempts(); got != 1 {
		t.Errorf("Attempts() = %d, want 1 (still the same open attempt)", got)
	}
	// It must still be possible to finish this attempt normally afterward.
	if err := j.Transition(Finalizing); err != nil {
		t.Fatalf("Transition(Finalizing): %v", err)
	}
	if _, err := j.Finish(OutcomeFailed, testClock()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

// TestJob_NeverRunReportsStateUnset replaces
// TestJob_NeverRunReportsWaitingForALease. A job with no attempt is not at a
// state; StateUnset says exactly that, where the old Waiting{Next: Fetching}
// claimed a position the job had not reached.
func TestJob_NeverRunReportsStateUnset(t *testing.T) {
	j := newTestJob(t)
	v := j.State()
	if v.State != StateUnset {
		t.Errorf("State() on a never-run job = %v, want StateUnset", v.State)
	}
	if v.Next != StateUnset {
		t.Errorf("Next = %v on a never-run job, want StateUnset: nothing has ended, so nothing is pending", v.Next)
	}
	if j.HasRun() {
		t.Error("HasRun() is true for a job with no attempt")
	}
}

// TestJob_SetNextRequiresAnOpenAttempt pins that the marker cannot be written
// before there is an attempt to carry it.
func TestJob_SetNextRequiresAnOpenAttempt(t *testing.T) {
	j := newTestJob(t)
	if err := j.SetNext(Assessing); !errors.Is(err, ErrNoOpenAttempt) {
		t.Errorf("SetNext on a never-run job, error = %v, want ErrNoOpenAttempt", err)
	}
}

func mustBegin(t *testing.T, j *Job) {
	t.Helper()
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
}

func mustJobTransition(t *testing.T, j *Job, to State) {
	t.Helper()
	if err := j.Transition(to); err != nil {
		t.Fatalf("Transition(%v): %v", to, err)
	}
}

// TestJob_WithOpenAttemptLease drives the adapter directly, because what it
// guarantees is not observable through Cross and Finish alone: those always
// return a lease on success and nil on error, so they cannot show that the
// adapter itself never invents one.
//
// Three properties, each of which a plausible rewrite would break:
//   - it refuses without calling fn at all, so a door cannot act on a settled
//     or never-run attempt before the check;
//   - it hands the callback the same *Attempt currentLocked resolves, not a
//     copy — a copy would silently discard every mutation;
//   - the returned lease is exactly what the callback returned, including nil
//     alongside an error. (*Lease, error) must never mean "failed, and here is
//     a lease".
func TestJob_WithOpenAttemptLease(t *testing.T) {
	t.Run("refuses without invoking fn", func(t *testing.T) {
		j := newTestJob(t)
		called := false
		l, err := j.withOpenAttemptLease(func(*Attempt) (*Lease, error) {
			called = true
			return &Lease{}, nil
		})
		if !errors.Is(err, ErrNoOpenAttempt) {
			t.Errorf("err = %v, want ErrNoOpenAttempt", err)
		}
		if called {
			t.Error("fn was invoked on a job with no open attempt")
		}
		if l != nil {
			t.Errorf("lease = %p, want nil — a refusal must yield nothing", l)
		}
	})

	t.Run("passes the live attempt and returns its lease", func(t *testing.T) {
		j := newTestJob(t)
		mustBegin(t, j)
		want := &Lease{}
		l, err := j.withOpenAttemptLease(func(a *Attempt) (*Lease, error) {
			// Fetched under the lock the adapter's callback already runs
			// beneath; taking it again here would deadlock.
			if got := j.currentLocked(); a != got {
				t.Errorf("fn got %p, want the attempt currentLocked resolves (%p)", a, got)
			}
			a.setActivity(ActPar2Verify)
			return want, nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if l != want {
			t.Errorf("lease = %p, want the one fn returned (%p)", l, want)
		}
		if got := j.State().Activity; got != ActPar2Verify {
			t.Errorf("Activity = %v; the callback received a copy, not the live attempt", got)
		}
	})

	t.Run("an error from fn yields no lease", func(t *testing.T) {
		j := newTestJob(t)
		mustBegin(t, j)
		sentinel := errors.New("probe")
		l, err := j.withOpenAttemptLease(func(*Attempt) (*Lease, error) {
			return nil, sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want the callback's error", err)
		}
		if l != nil {
			t.Errorf("lease = %p, want nil on an error path", l)
		}
	})
}

// currentCrossedForTest reports the current attempt's crossed() under the
// job's own read lock. The reachability walk needs it because crossed() lives
// on Attempt and the walk only holds a *Job; going through State() would not
// do, since crossed is deliberately not on StateView — nothing outside this
// package needs it.
func (j *Job) currentCrossedForTest() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	a := j.currentLocked()
	return a != nil && a.crossed()
}

// TestJob_SnapshotIsAtomic pins that a snapshot is taken under one lock
// acquisition, by racing it against the doors and requiring the result be
// internally consistent at every observation.
//
// The consistency rule chosen is the one the Queue actually depends on: a
// settled attempt has HasRun true, and a job that has never run has a
// StateUnset position with a Pending outcome. Composing State() and HasRun()
// separately can observe HasRun false with a real position, which is a job
// configuration that has never existed.
func TestJob_SnapshotIsAtomic(t *testing.T) {
	j := newTestJob(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2000 {
			_ = j.BeginAttempt(testClock())
			_ = j.Transition(Assessing)
			_, _ = j.Finish(OutcomeFailed, testClock())
		}
	}()
	for range 20000 {
		s := j.Snapshot()
		// The invariant, as a BICONDITIONAL: a job has run if and only if it is
		// at a position. newAttempt starts an attempt at Fetching, and settling
		// no longer moves it, so every job with an attempt has a position and
		// every job without one has StateUnset.
		//
		// Stated both ways deliberately. An earlier draft asserted only
		// `position set && !HasRun`, which is VACUOUS: Go evaluates a composite
		// literal's fields left to right, so the torn version reads HasRun
		// strictly after State, and len(attempts) never shrinks — so HasRun can
		// never be observed false once a position has been seen. The tear that
		// IS observable is the other direction, State read before the first
		// append and HasRun after it. A biconditional catches both and does not
		// depend on the field order, which a reader should not have to reason
		// about to trust this test.
		if s.HasRun != (s.State.State != StateUnset) {
			t.Fatalf("torn snapshot: HasRun=%v but position=%v; a job has run if and only if "+
				"it is at a position", s.HasRun, s.State.State)
		}
	}
	<-done
}

// TestSnapshot_IsOpen covers the predicate in two complementary ways, because
// each alone leaves a real gap.
//
// The door-driven half proves the three shapes a real job actually reaches, so
// the predicate is exercised against configurations the machine produces
// rather than ones a test invented. The total half walks every Outcome and
// pins the arithmetic — IsOpen is HasRun AND not settled — which a
// three-case test cannot, since a new Outcome would slip through it silently.
// Snapshot is a plain value and IsOpen reads two of its fields, so
// constructing one here tests the predicate rather than the machine; that is
// the point of splitting them.
func TestSnapshot_IsOpen(t *testing.T) {
	t.Run("shapes a real job reaches", func(t *testing.T) {
		j := newTestJob(t)
		if s := j.Snapshot(); s.IsOpen() {
			t.Errorf("IsOpen() = true for a job that has never run (HasRun=%v)", s.HasRun)
		}
		mustBegin(t, j)
		if s := j.Snapshot(); !s.IsOpen() {
			t.Errorf("IsOpen() = false for a begun, unsettled attempt (HasRun=%v, outcome=%v)",
				s.HasRun, s.State.Outcome)
		}
		if _, err := j.Finish(OutcomeFailed, testClock()); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		if s := j.Snapshot(); s.IsOpen() {
			t.Errorf("IsOpen() = true for a settled attempt (outcome=%v)", s.State.Outcome)
		}
	})

	t.Run("total over HasRun x Outcome", func(t *testing.T) {
		for _, hasRun := range []bool{false, true} {
			for _, o := range AllOutcomes() {
				s := Snapshot{HasRun: hasRun, State: StateView{Outcome: o}}
				want := hasRun && !o.IsSettled()
				if got := s.IsOpen(); got != want {
					t.Errorf("Snapshot{HasRun:%v, Outcome:%v}.IsOpen() = %v, want %v",
						hasRun, o, got, want)
				}
			}
		}
	})
}
