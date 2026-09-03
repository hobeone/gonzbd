package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestDispatcherControlSurface_PerJobDoors pins the doors the API needs and
// did not have: a job pointer, and per-job pause/resume distinct from the
// queue-wide flag.
func TestDispatcherControlSurface_PerJobDoors(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := d.Job("a")
	if !ok || got.ID() != "a" {
		t.Fatalf("Job(a) = %v, %v; want the job", got, ok)
	}

	if err := d.PauseJob("a"); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	if in := j.Intent(); in != job.IntentPause {
		t.Fatalf("Intent = %v, want IntentPause", in)
	}

	// Per-job pause must NOT set the queue-wide flag. Conflating the two is
	// what ToSABnzbd's WaitReason.IsPause() routing exists to survive, and a
	// control surface that sets both makes that distinction unobservable.
	if d.Paused() {
		t.Fatal("PauseJob must not set the queue-wide pause flag")
	}

	if err := d.ResumeJob("a"); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if in := j.Intent(); in != job.IntentRun {
		t.Fatalf("Intent = %v, want IntentRun", in)
	}

	if _, ok := d.Job("nope"); ok {
		t.Fatal("Job of an unknown id must report not-found")
	}
	if err := d.PauseJob("nope"); err == nil {
		t.Fatal("PauseJob of an unknown id must error")
	}
	if err := d.ResumeJob("nope"); err == nil {
		t.Fatal("ResumeJob of an unknown id must error")
	}
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := d.ResumeJob("a"); err == nil {
		t.Fatal("ResumeJob on cancelled job must error")
	}
}

// TestDispatcherRemove_IsIdempotentAndReturnsResources pins that Remove gives
// back what the job held. A removed job that keeps its lease or slot strands
// pool capacity for the life of the process, and nothing later reclaims it --
// the tick only walks registered jobs.
func TestDispatcherRemove_IsIdempotentAndReturnsResources(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withCaps(1, 1), withStore(st))
	jA := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(jA, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add(a): %v", err)
	}
	jB := job.New("b", "Job B", job.PolicyFromPP(3))
	if err := d.Add(jB, Header{Name: "Job B"}); err != nil {
		t.Fatalf("Add(b): %v", err)
	}

	// Tick to grant lease to job A (capacity 1). Two ticks: first opens attempt, second grants lease.
	d.tick(context.Background())
	d.tick(context.Background())
	if !jA.HoldsLease() {
		t.Fatal("precondition: job A must hold the lease")
	}
	if jB.HoldsLease() {
		t.Fatal("precondition: job B must not hold lease when capacity is 1")
	}

	if err := d.Remove(context.Background(), "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !st.deleted("a") {
		t.Fatal("Remove must delete the job from the store")
	}
	if _, ok := d.Job("a"); ok {
		t.Fatal("Remove must deregister the job")
	}
	if err := d.Remove(context.Background(), "a"); err == nil {
		t.Fatal("Remove of an already-removed job must error, not silently succeed")
	}

	// Next tick must advance job B since A returned its lease on Remove.
	d.tick(context.Background())
	if !jB.HoldsLease() {
		t.Fatal("Remove failed to return lease capacity: job B did not acquire lease on next tick")
	}

	stErr := &fakeStore{delErr: errors.New("disk is angry")}
	dErr := newTestDispatcher(t, withStore(stErr))
	jErr := job.New("e", "Job E", job.PolicyFromPP(3))
	if err := dErr.Add(jErr, Header{Name: "Job E"}); err != nil {
		t.Fatalf("Add(e): %v", err)
	}
	if err := dErr.Remove(context.Background(), "e"); err == nil {
		t.Fatal("Remove must error if store.Delete fails")
	}
}
