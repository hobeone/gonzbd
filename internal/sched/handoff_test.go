package sched

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestHandoff_AtFromRecordsNextParksAndCallsHanded pins the acting path: next
// is recorded, both pools are returned, and handed runs.
func TestHandoff_AtFromRecordsNextParksAndCallsHanded(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)

	calls := 0
	handed, err := q.Handoff(j, job.Assessing, job.Repairing, func() { calls++ })
	if err != nil || !handed {
		t.Fatalf("Handoff = (%v, %v), want (true, nil)", handed, err)
	}
	if got := j.Snapshot().State.Next; got != job.Repairing {
		t.Errorf("Next = %v, want Repairing", got)
	}
	if j.HoldsLease() || q.slots.holds(j.ID()) {
		t.Errorf("Handoff left resources held: lease %v, slot %v", j.HoldsLease(), q.slots.holds(j.ID()))
	}
	if calls != 1 {
		t.Errorf("handed called %d times, want 1", calls)
	}
}

// TestHandoff_StaleReportTouchesNothing pins the state check: a report for a
// state the job has left, or for one whose next is already recorded, must
// leave the job and its resources exactly as they are and not call handed.
func TestHandoff_StaleReportTouchesNothing(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, q *Queue, j *job.Job)
		from  job.State
		next  job.State
	}{
		{"moved on", func(t *testing.T, q *Queue, j *job.Job) {
			t.Helper()
			mustAdvanceTo(t, q, j, job.Assessing)
		}, job.Fetching, job.Assessing},
		{"next already recorded", func(t *testing.T, q *Queue, j *job.Job) {
			t.Helper()
			mustAdvanceTo(t, q, j, job.Assessing)
			if err := j.SetNext(job.Repairing); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
		}, job.Assessing, job.Repairing},
		{"never run", func(*testing.T, *Queue, *job.Job) {}, job.StateUnset, job.Fetching},
		{"settled", func(t *testing.T, q *Queue, j *job.Job) {
			t.Helper()
			mustAdvanceToSettled(t, q, j, job.OutcomeFailed)
		}, job.Fetching, job.Assessing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := New(1, 1, testClock, &stubWorkers{})
			j := job.New("j1", "n", job.Policy{})
			tc.setup(t, q, j)
			before := j.Snapshot()
			slot := q.slots.holds(j.ID())

			called := false
			handed, err := q.Handoff(j, tc.from, tc.next, func() { called = true })
			if handed || err != nil {
				t.Errorf("Handoff = (%v, %v), want (false, nil)", handed, err)
			}
			if called {
				t.Error("handed called for a stale report")
			}
			if after := j.Snapshot(); after != before {
				t.Errorf("snapshot changed: %+v -> %+v", before, after)
			}
			if q.slots.holds(j.ID()) != slot {
				t.Errorf("slot held changed from %v", slot)
			}
		})
	}
}

// TestHandoff_RefusedVerdictSettlesFailed pins the refusal path: the job
// settles OutcomeFailed with its resources returned, and handed still runs.
// Parking instead would relaunch the same state to report the same refused
// verdict again.
func TestHandoff_RefusedVerdictSettlesFailed(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)

	called := false
	handed, err := q.Handoff(j, job.Assessing, job.Finalizing, func() { called = true })
	if !handed {
		t.Fatal("Handoff did not act on a job at from")
	}
	if !errors.Is(err, job.ErrIllegalTransition) {
		t.Errorf("err = %v, want the SetNext refusal", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeFailed {
		t.Errorf("outcome = %v, want Failed", got)
	}
	if j.HoldsLease() || q.slots.holds(j.ID()) {
		t.Errorf("a refused verdict left resources held: lease %v, slot %v", j.HoldsLease(), q.slots.holds(j.ID()))
	}
	if !called {
		t.Error("handed not called")
	}
}

// TestHandoff_NoVerdictParksWithoutRecordingNext pins the yield form: with
// next == StateUnset, Handoff parks and calls handed but records nothing.
func TestHandoff_NoVerdictParksWithoutRecordingNext(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Fetching)

	called := false
	handed, err := q.Handoff(j, job.Fetching, job.StateUnset, func() { called = true })
	if !handed || err != nil {
		t.Fatalf("Handoff = (%v, %v), want (true, nil)", handed, err)
	}
	if s := j.Snapshot(); s.State.Next != job.StateUnset || s.State.Outcome.IsSettled() {
		t.Errorf("a yield recorded something: %+v", s.State)
	}
	if j.HoldsLease() {
		t.Error("a yield left the lease held")
	}
	if !called {
		t.Error("handed not called")
	}
}

// TestHandoff_HandedRunsInsideTheQueueLock pins the property the dispatcher's
// claim release depends on: handed runs before q.mu is released, so no
// Advance can land between the park and it.
func TestHandoff_HandedRunsInsideTheQueueLock(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Fetching)

	locked := false
	if _, err := q.Handoff(j, job.Fetching, job.Assessing, func() {
		if q.mu.TryLock() {
			q.mu.Unlock()
			return
		}
		locked = true
	}); err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	if !locked {
		t.Error("handed ran with q.mu free")
	}
}

// TestHandoff_NilHandedIsAllowed pins that handed is optional.
func TestHandoff_NilHandedIsAllowed(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Fetching)
	if handed, err := q.Handoff(j, job.Fetching, job.Assessing, nil); !handed || err != nil {
		t.Fatalf("Handoff = (%v, %v), want (true, nil)", handed, err)
	}
}
