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
// a job that HAS run: that job settles OutcomeCancelled and must stay visible
// in the listing so the user can see what happened to it.
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
	d.tick(context.Background())

	if got := d.List(); len(got) != 1 {
		t.Errorf("List has %d rows, want 1 — a job that HAS run must settle as Cancelled and stay visible; only the never-run case is evicted", len(got))
	}
}
