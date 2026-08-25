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
	got := j.currentLocked()
	j.mu.RUnlock()
	if got == nil {
		t.Fatal("currentLocked() after BeginAttempt = nil, want the open attempt")
	}

	called := false
	if err := j.withOpenAttempt(func(a *Attempt) error {
		called = true
		if a != got {
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

// TestJob_NeverRunReportsWaitingForALease pins D1: there is no Queued state,
// and a job that has never run has no attempt record at all.
func TestJob_NeverRunReportsWaitingForALease(t *testing.T) {
	j := newTestJob(t)
	if j.HasRun() {
		t.Error("HasRun() = true on a fresh job, want false")
	}
	if got := j.Attempts(); got != 0 {
		t.Errorf("Attempts() = %d, want 0", got)
	}
	v := j.State()
	if v.State != Waiting || v.Next != Fetching || v.Reason != NoLease {
		t.Errorf("State() = %+v; want State=Waiting Next=Fetching Reason=NoLease", v)
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

// TestJob_BeginAttemptIsIdempotentWhileOneIsOpen pins the rule that a lease
// re-issued after a pause does not count as a new attempt.
func TestJob_BeginAttemptIsIdempotentWhileOneIsOpen(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if err := j.Hold(Fetching, UserPaused); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := j.BeginAttempt(testClock().Add(time.Hour)); err != nil {
		t.Fatalf("second BeginAttempt: %v", err)
	}
	if got := j.Attempts(); got != 1 {
		t.Errorf("Attempts() = %d after a pause/resume cycle, want 1; "+
			"an attempt closes only at Finished", got)
	}
}

// TestJob_RetryAppendsAnAttempt is D2's core property: the previous verdict
// survives, and the new attempt starts pending.
func TestJob_RetryAppendsAnAttempt(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if err := j.Finish(OutcomeUnrecoverable, testClock()); err != nil {
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
		{"Hold", func(j *Job) error { return j.Hold(Fetching, UserPaused) }},
		{"SetActivity", func(j *Job) error { return j.SetActivity(ActUnpack) }},
		{"Finish", func(j *Job) error { return j.Finish(OutcomeOK, testClock()) }},
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
	if err := j.Finish(OutcomeOK, testClock()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := j.Transition(Fetching); !errors.Is(err, ErrNoOpenAttempt) {
		t.Errorf("Transition after Finish = %v, want ErrNoOpenAttempt", err)
	}
}

// TestJob_TransitionSurfacesHoldAndFinishDoors pins that Job.Transition does
// not translate, wrap, or swallow the three-doors sentinels from Attempt —
// it surfaces them unchanged.
func TestJob_TransitionSurfacesHoldAndFinishDoors(t *testing.T) {
	j := newTestJob(t)
	mustBegin(t, j)
	if err := j.Transition(Waiting); !errors.Is(err, ErrHoldRequired) {
		t.Errorf("Transition(Waiting) = %v, want ErrHoldRequired", err)
	}
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

func mustBegin(t *testing.T, j *Job) {
	t.Helper()
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
}
