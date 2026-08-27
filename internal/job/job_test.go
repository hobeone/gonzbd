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
// BeginAttempt on a job whose current attempt is still open (not Finished) is
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
			"an attempt closes only at Finished", got)
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
		{"Finish", func(j *Job) error { _, err := j.Finish(OutcomeOK, testClock()); return err }},
		// Cross belongs here even though it can never succeed from a never-run
		// job for a second reason (it is legal only from Assessing): the door
		// checks for an open attempt FIRST, and it is one of the two doors
		// that check inline rather than through withOpenAttempt, so it is
		// exactly the one that could drift from the other four unnoticed.
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

func TestJob_FinishedJobHasNoOpenAttempt(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if _, err := j.Finish(OutcomeOK, testClock()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := j.Transition(Fetching); !errors.Is(err, ErrNoOpenAttempt) {
		t.Errorf("Transition after Finish = %v, want ErrNoOpenAttempt", err)
	}
}

// TestJob_TransitionSurfacesFinishRequired pins that Job.Transition does not
// translate, wrap, or swallow Attempt's ErrFinishRequired — it surfaces it
// unchanged. Waiting is gone, so ErrHoldRequired's half of what this test
// used to cover no longer has a door to pin: finish is now the only
// non-work-spine door transition still refuses on Attempt's behalf.
func TestJob_TransitionSurfacesFinishRequired(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if err := j.Transition(Finished); !errors.Is(err, ErrFinishRequired) {
		t.Errorf("Transition(Finished) = %v, want ErrFinishRequired", err)
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
// Extracting, then Finished with OutcomeOK, let BeginAttempt open a second
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
	if _, err := j.Finish(OutcomeOK, testClock()); err != nil {
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
	if got := j.State().State; got != Finished {
		t.Errorf("State() after refused retry = %v, want Finished (unchanged)", got)
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
	if _, err := j.Finish(OutcomeOK, testClock()); err != nil {
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
