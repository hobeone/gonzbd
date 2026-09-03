package dispatch

import (
	"context"
	"fmt"
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

	// Per-job pause must NOT set the queue-wide pause flag. Conflating the two is
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
}

// TestDispatcherRemove_IsIdempotentAndReturnsResources pins that Remove gives
// back what the job held. A removed job that keeps its lease or slot strands
// pool capacity for the life of the process, and nothing later reclaims it --
// the tick only walks registered jobs.
//
// The brief's own version of this test (Cancel first, so d.q.Cancel(j) runs
// against the *job.Job pointer directly) turned out NOT to discriminate the
// [Remove deregisters before cancelling] mutation: sched.Queue.Cancel takes
// the job pointer itself, not a lookup through the dispatcher's registry, so
// reordering d.remove(id) ahead of it changes nothing about which lease or
// slot gets released on the SUCCESS path -- confirmed by first adding a
// lease-capacity assertion (job "b" only obtaining "a"'s returned lease
// under leaseCap 1) and observing it pass under the mutation too, i.e. it
// also did not kill it. `go run ./scripts/mutate
// internal/dispatch/testdata/control_surface.spec` still reports SURVIVED
// for that version -- see the task report for the transcript.
//
// What the mutated order DOES change is the failure path: it deregisters the
// job before Cancel or store.Delete have had a chance to fail, so a failing
// Remove silently drops the job from the registry anyway -- the caller's
// error return is the only trace, List/Job/Row all read as if the removal
// had succeeded, and nothing revisits it (the tick only walks registered
// jobs). storeErr below forces store.Delete to fail so this is observable:
// the job must remain registered after a failed Remove.
func TestDispatcherRemove_IsIdempotentAndReturnsResources(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("a", "Job A", job.PolicyFromPP(3))
	if err := d.Add(j, Header{Name: "Job A"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := d.Remove(context.Background(), "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := d.Job("a"); ok {
		t.Fatal("Remove must deregister the job")
	}
	if err := d.Remove(context.Background(), "a"); err == nil {
		t.Fatal("Remove of an already-removed job must error, not silently succeed")
	}

	// A Remove that fails partway must leave the job registered, so a caller
	// can inspect or retry it. If deregistration happens before the failing
	// step, as the mutation does, the job vanishes from the registry despite
	// the error -- indistinguishable from a successful removal to any later
	// caller of Job/Row/List.
	storeErr := &fakeStore{delErr: fmt.Errorf("store: delete failed")}
	d2 := newTestDispatcher(t, withStore(storeErr))
	b := job.New("b", "Job B", job.PolicyFromPP(3))
	if err := d2.Add(b, Header{Name: "Job B"}); err != nil {
		t.Fatalf("Add(b): %v", err)
	}
	if err := d2.Remove(context.Background(), "b"); err == nil {
		t.Fatal("Remove must report the store.Delete failure, not swallow it")
	}
	if _, ok := d2.Job("b"); !ok {
		t.Fatal("a Remove that failed must leave the job registered -- deregistering it anyway strands it: gone from Job/Row/List, but store.Delete never actually removed the persisted row")
	}
}
