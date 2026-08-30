package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// failingDeleteStore wraps fakeStore and makes Delete always fail, for
// TestCancelledNeverRunJob_StoreFailure_StaysRegistered. It embeds fakeStore
// so Load and Save behave normally and deleted() still reports what was
// actually removed (nothing, since Delete never succeeds).
type failingDeleteStore struct {
	fakeStore
}

func (f *failingDeleteStore) Delete(context.Context, string) error {
	return errors.New("boom")
}

// TestCancelledNeverRunJob_IsRemovedFromTheListing pins D-B12: a job cancelled
// before it ever ran must not survive in the listing. Without eviction,
// finishCancel returns nil for it (Outcome lives on the Attempt and there is
// none), the job keeps StateUnset, and job.ToSABnzbd renders it StatusQueued
// forever.
func TestCancelledNeverRunJob_IsRemovedFromTheListing(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	d.tick(context.Background())

	if got := d.List(); len(got) != 0 {
		t.Errorf("List has %d rows after cancelling a never-run job, want 0 — finishCancel cannot settle one (Outcome lives on the Attempt and there is none), so without eviction it renders as StatusQueued forever", len(got))
	}
}

// TestCancelledNeverRunJob_IsDeletedFromTheStore pins that eviction removes
// the persisted row too — leaving it would resurrect the job at the next
// Start.
func TestCancelledNeverRunJob_IsDeletedFromTheStore(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	d.tick(context.Background())

	if !st.deleted("j1") {
		t.Error("job removed from the registry but not the store — it would come back at the next Start")
	}
}

// TestCancelledNeverRunJob_StoreFailure_StaysRegistered pins that a
// store.Delete failure leaves the job registered rather than dropping it
// locally: doing otherwise would desync the registry from the store — the
// row survives on disk and the job would come back at the next Start, which
// is worse than trying the eviction again on the next tick.
func TestCancelledNeverRunJob_StoreFailure_StaysRegistered(t *testing.T) {
	st := &failingDeleteStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	d.tick(context.Background())

	if got := d.List(); len(got) != 1 {
		t.Errorf("List has %d rows after a failed store.Delete, want 1 — the job must stay registered when the store write fails", len(got))
	}
	if st.deleted("j1") {
		t.Error("deleted(j1) is true, but Delete always fails in this test")
	}
}

// TestEvictCancelledNeverRun_CalledDirectly exercises evictCancelledNeverRun
// on its own, mirroring the pattern TestReconcileResidency_CalledDirectlyHydrates
// uses for its sibling helper in residency_test.go, so it has a direct test in
// addition to the tick-driven behavioural tests above.
func TestEvictCancelledNeverRun_CalledDirectly(t *testing.T) {
	st := &fakeStore{}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := d.q.Advance(j); err != nil { // routes IntentCancel to finishCancel's StateUnset arm: no-op
		t.Fatalf("Advance (cancel): %v", err)
	}

	if !d.evictCancelledNeverRun(context.Background(), j) {
		t.Fatal("evictCancelledNeverRun called directly did not evict a cancelled never-run job")
	}
	if !st.deleted("j1") {
		t.Error("evictCancelledNeverRun did not delete the store row")
	}
	if got := d.List(); len(got) != 0 {
		t.Errorf("List has %d rows after evictCancelledNeverRun, want 0", len(got))
	}
}

// TestCancelledRunningJob_IsNotEvicted distinguishes the never-run case from
// a job that HAS run: eviction is gated on StateUnset, so a job past that
// point stays in the listing where the user can see what happened to it.
//
// The worker yield is what makes the settle reachable, and it is the whole
// reason this test can assert one. finishCancel (internal/sched/cancel.go)
// sees a RUNNING pre-boundary job, calls work.Abort and returns — "settled on
// the tick after it yields" — and stubWorkers.Abort only records the ID.
// Without d.Yielded nothing ever yields, so the job stays Pending forever and
// the listing count alone passes whenever it is still registered for ANY
// reason, including a cancel that never latched.
//
// TestCancelledWorker_SettlesRatherThanReAbortingForever
// (internal/dispatch/worker_test.go) covers the same yield for the
// re-Abort-loop property; here it is setup for the eviction question, and the
// intent and state assertions name the two facts that make eviction
// inapplicable.
func TestCancelledRunningJob_IsNotEvicted(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !d.q.Render(j).Running {
		t.Fatal("setup: job is not running")
	}

	if err := d.Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// The worker notices the Abort and exits without finishing.
	if err := d.Yielded(j.ID()); err != nil {
		t.Fatalf("Yielded: %v", err)
	}
	d.tick(context.Background())

	v := d.q.Render(j)
	if v.Outcome != job.OutcomeCancelled {
		t.Errorf("Outcome = %v, want Cancelled — a job that HAS run settles "+
			"through finishCancel on the tick after its worker yields", v.Outcome)
	}
	if v.Intent != job.IntentCancel {
		t.Errorf("Intent = %v, want IntentCancel — the cancel must have latched, or the job is still listed for the wrong reason", v.Intent)
	}
	if v.State == job.StateUnset {
		t.Errorf("State = StateUnset — this job HAS run, and StateUnset is the gate evictCancelledNeverRun keys on; if it reads unset the test is no longer covering the case it names")
	}
	if got := d.List(); len(got) != 1 {
		t.Errorf("List has %d rows, want 1 — a job that HAS run must settle as Cancelled and stay visible; only the never-run case is evicted", len(got))
	}
}

// TestEvictCancelledNeverRun_DeleteFailureDoesNotResurrectTheRow pins the
// interaction between the two halves of the eviction path.
//
// evictCancelledNeverRun returns false when store.Delete fails, deliberately
// leaving the job registered so a later tick can retry — removing it from the
// registry while the store still holds its row would resurrect it at the next
// Start. But false is also what it returns for a job that is simply not a
// cancelled never-run one, so tick could not tell the two apart and walked on
// to persistIfChanged. The job's Intent had just changed to IntentCancel, so
// it no longer matched lastWritten and Save wrote it straight back — the tick
// re-created the row whose deletion had just failed, in the same pass.
func TestEvictCancelledNeverRun_DeleteFailureDoesNotResurrectTheRow(t *testing.T) {
	st := &fakeStore{delErr: errors.New("disk is angry")}
	d := newTestDispatcher(t, withStore(st))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	d.tick(context.Background())

	if got := d.q.Render(j).State; got != job.StateUnset {
		t.Fatalf("setup: State = %v, want StateUnset — this must be the never-run case", got)
	}
	if _, ok := st.row("j1"); ok {
		t.Error("the store holds a row for a cancelled never-run job whose " +
			"Delete failed — the tick walked on to persistIfChanged and wrote " +
			"back the row it had just failed to remove")
	}
	if got := len(d.List()); got != 1 {
		t.Errorf("List has %d rows, want 1 — the job must stay registered so a "+
			"later tick can retry the delete", got)
	}
}
