package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// The teardown window these tests stand in.
//
// Both deregistration paths do the same three things in the same order: check
// that nothing is live, delete the store row with d.mu RELEASED, then erase
// the job's bookkeeping. The middle step is the window: it is the only part of
// a removal that is neither instantaneous nor under d.mu, and every guard that
// is supposed to keep new work off a job being torn down has to hold across
// it.
//
// hookStore.beforeDelete is how a test gets inside that window and holds it
// open. Without it the window is real but unobservable — it opens and closes
// inside one unlocked call, so a test racing it from another goroutine would
// be pinning the scheduler rather than the protocol.
//
// The eviction runs on its own goroutine and the probes run on the test's,
// which is the shape that matters: a probe called from INSIDE the hook would
// run on the evicting goroutine, and a guard can hardly race a caller it is
// sequenced with. The first version of this test did exactly that and passed
// for the wrong reason.
//
// #513 is that Dispatcher.Remove marks the job for the whole window and
// evictCancelledNeverRun did not, so the guards below passed for a job whose
// bookkeeping was about to be erased underneath whatever they admitted.

// cancelledNeverRun registers a job and drives it to the state
// evictCancelledNeverRun acts on: StateUnset with IntentCancel latched.
func cancelledNeverRun(t *testing.T, d *Dispatcher, id string) *job.Job {
	t.Helper()
	j := job.New(id, "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := d.q.Advance(j); err != nil { // IntentCancel + StateUnset routes to finishCancel's no-op arm
		t.Fatalf("Advance (cancel): %v", err)
	}
	return j
}

// TestEvictCancelledNeverRun_RefusesNewWorkDuringTeardown pins the invariant
// #513 names: a job being deregistered must not admit new work.
//
// Before the fix this test fails by ADMITTING it — Occupy returns nil, having
// written into d.occupiers and d.occupancyTokens for a job whose bookkeeping
// the caller is three lines from erasing. The consequence is not a leak but a
// lie: deregister drops occupancyTokens wholesale, so every later liveness
// query (waitLive, IsOccupied, evictCancelledNeverRun's own check) reports
// that nothing is running on a job that is still executing fn.
//
// It covers both guards #513 names in one window, because it is one window:
// Occupy and claimLaunched consult the same predicate, and a worker admitted
// here is erased the same way an occupier is — deregister calls clearLaunched,
// dropping the latch that stops a SECOND worker while the first still runs.
func TestEvictCancelledNeverRun_RefusesNewWorkDuringTeardown(t *testing.T) {
	deleteStarted := make(chan struct{})
	release := make(chan struct{})
	st := &hookStore{
		Store: &fakeStore{},
		beforeDelete: func(string) {
			close(deleteStarted)
			<-release
		},
	}
	d := newTestDispatcher(t, withStore(st))
	j := cancelledNeverRun(t, d, "j1")

	// Releasing from Cleanup as well as below keeps a failed assertion from
	// leaving the eviction goroutine parked on a channel nobody closes.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	evicted := make(chan bool, 1)
	go func() { evicted <- d.evictCancelledNeverRun(context.Background(), j) }()

	select {
	case <-deleteStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the store delete to begin")
	}

	// Inside the window, on a different goroutine from the eviction.
	var ran bool
	occupyErr := d.Occupy(context.Background(), "j1", func(context.Context) { ran = true })
	claimed := d.claimLaunched("j1")

	close(release)
	select {
	case ok := <-evicted:
		if !ok {
			t.Error("evictCancelledNeverRun returned false, want true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for evictCancelledNeverRun to finish")
	}

	if !errors.Is(occupyErr, ErrNotFound) {
		t.Errorf("Occupy during the teardown window returned %v, want ErrNotFound — "+
			"the job is mid-removal and its occupancy bookkeeping is about to be erased", occupyErr)
	}
	if ran {
		t.Error("Occupy ran its function for a job being deregistered — deregister erases " +
			"occupancyTokens, so this lease is invisible to every liveness query the moment it is granted")
	}
	if claimed {
		t.Error("claimLaunched succeeded for a job being deregistered — deregister calls clearLaunched, " +
			"so the latch preventing a second worker is dropped while the first still runs")
	}
}

// TestEvictCancelledNeverRun_StoreFailureReadmitsWork pins the hazard the fix
// itself introduces, and is the reason the marker needs an explicit release
// rather than relying on deregister to delete the map entry.
//
// A failed store.Delete deliberately leaves the job REGISTERED so the next
// tick can retry (TestCancelledNeverRunJob_StoreFailure_StaysRegistered pins
// that). deregister is not reached on that path, so nothing would clear the
// marker: the job would stay registered but permanently refuse work, and the
// retry the error path exists for would evict a job that had silently stopped
// being schedulable. Registered-and-inert is a worse failure than the store
// error it followed, because nothing reports it.
func TestEvictCancelledNeverRun_StoreFailureReadmitsWork(t *testing.T) {
	st := &fakeStore{delErr: errors.New("boom")}
	d := newTestDispatcher(t, withStore(st))
	j := cancelledNeverRun(t, d, "j1")

	if !d.evictCancelledNeverRun(context.Background(), j) {
		t.Fatal("evictCancelledNeverRun returned false after a store failure; it must report the pass over")
	}
	if got := d.List(); len(got) != 1 {
		t.Fatalf("List has %d rows after a failed store.Delete, want 1", len(got))
	}

	// The job is still registered, so it must still behave like a registered
	// job. Occupy is the sharpest probe: it is the guard #513 is about.
	if err := d.Occupy(context.Background(), "j1", func(context.Context) {}); err != nil {
		t.Errorf("Occupy after a failed store.Delete returned %v, want nil — the job stayed "+
			"registered for the retry, so the removal marker must have been released with it", err)
	}
	if !d.claimLaunched("j1") {
		t.Error("claimLaunched after a failed store.Delete returned false — the job stayed registered " +
			"for the retry, so it must still be launchable")
	}
}
