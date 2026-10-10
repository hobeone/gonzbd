package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
)

// countingResidency is a fakeResidency that counts Hydrate calls, so a test
// can assert that a resume reused the manifest rather than re-reading it.
func countingResidency() (*fakeResidency, *atomic.Int32) {
	var n atomic.Int32
	return &fakeResidency{onHydrate: func(string) { n.Add(1) }}, &n
}

// fetchingWithLease adds a job and ticks it to Fetching holding a lease, with
// its manifest hydrated and its Fetching worker launched.
func fetchingWithLease(t *testing.T, d *Dispatcher, id string) *job.Job {
	t.Helper()
	j := job.New(id, id, job.Policy{})
	if err := d.Add(context.Background(), j, Header{Name: id}); err != nil {
		t.Fatalf("Add(%s): %v", id, err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if v := d.q.Render(j); !v.Running || v.State != job.Fetching {
		t.Fatalf("fixture: %s is not running at Fetching: %+v", id, v)
	}
	return j
}

// TestPauseJob_FetchingJobFreesItsLease pins that a per-job pause returns a
// Fetching job's lease. The downloader stops serving a paused job but reports
// nothing, and Advance never strips a lease from a holder, so without the
// pause's own yield the paused job kept one of leaseCap leases and, at a cap
// of one, the job queued behind it never started.
func TestPauseJob_FetchingJobFreesItsLease(t *testing.T) {
	r := &fakeRunner{}
	d := newTestDispatcher(t, withCaps(1, 1), withRunner(r))
	a := fetchingWithLease(t, d, "a")
	b := job.New("b", "b", job.Policy{})
	if err := d.Add(context.Background(), b, Header{Name: "b"}); err != nil {
		t.Fatalf("Add(b): %v", err)
	}
	d.tick(context.Background())
	if b.HoldsLease() {
		t.Fatal("fixture: b holds a lease while a holds the only one")
	}

	if err := d.PauseJob("a"); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	if a.HoldsLease() {
		t.Fatal("the paused Fetching job still holds its lease")
	}
	d.tick(context.Background())

	if !b.HoldsLease() || !r.started("b") {
		t.Errorf("b holds lease = %v, launched = %v after a's pause, want both: the "+
			"paused job's lease was not returned to the pool", b.HoldsLease(), r.started("b"))
	}
	if v := d.q.Render(a); v.Running || job.ToSABnzbd(v) != constants.StatusPaused {
		t.Errorf("a renders Running=%v status %q, want not running and Paused", v.Running, job.ToSABnzbd(v))
	}
}

// TestPauseJob_RefusesACancelledJobAndYieldsNothing pins that a pause the
// cancel latch refuses changes nothing: the error comes back and the job's
// worker keeps its lease, since only the cancel's own path may release it.
func TestPauseJob_RefusesACancelledJobAndYieldsNothing(t *testing.T) {
	d := newTestDispatcher(t)
	j := fetchingWithLease(t, d, "a")
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}

	if err := d.PauseJob("a"); !errors.Is(err, job.ErrIntentLatched) {
		t.Fatalf("PauseJob on a cancelled job = %v, want ErrIntentLatched", err)
	}
	if !j.HoldsLease() {
		t.Error("a refused pause took the job's lease")
	}
}

// TestPauseJob_KeepsThePausedJobResident pins the residency half: a job paused
// while resident keeps its manifest after it gives back its lease, because
// fetches it dispatched before the pause can still land on it.
func TestPauseJob_KeepsThePausedJobResident(t *testing.T) {
	res, _ := countingResidency()
	d := newTestDispatcher(t, withResidency(res))
	j := fetchingWithLease(t, d, "a")

	if err := d.PauseJob("a"); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	d.tick(context.Background())

	if j.HoldsLease() {
		t.Fatal("fixture: the pause did not return the lease, so residency was not tested without one")
	}
	if !res.resident("a") || !d.isResident("a") {
		t.Errorf("paused job resident = %v, recorded resident = %v, want both: its "+
			"manifest was evicted with fetches still in flight", res.resident("a"), d.isResident("a"))
	}
	// A download that completes after the pause is still reported and recorded.
	if err := d.AdvanceFrom(j, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom after the pause: %v", err)
	}
	if got := j.Snapshot().State.Next; got != job.Assessing {
		t.Errorf("Next = %v after a completion landed on the paused job, want Assessing", got)
	}
}

// TestResumeJob_ContinuesWithoutRehydrating pins that a resume reuses the
// manifest the pause kept: the next tick grants a lease and launches the
// Fetching worker, and Hydrate is not called again.
func TestResumeJob_ContinuesWithoutRehydrating(t *testing.T) {
	res, hydrations := countingResidency()
	r := &fakeRunner{}
	d := newTestDispatcher(t, withResidency(res), withRunner(r))
	j := fetchingWithLease(t, d, "a")
	if got := hydrations.Load(); got != 1 {
		t.Fatalf("fixture: %d hydrations before the pause, want 1", got)
	}

	if err := d.PauseJob("a"); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	d.tick(context.Background())
	if err := d.ResumeJob("a"); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	d.tick(context.Background())

	if v := d.q.Render(j); !v.Running || v.State != job.Fetching {
		t.Fatalf("after the resume a is %+v, want running at Fetching", v)
	}
	if got := hydrations.Load(); got != 1 {
		t.Errorf("%d hydrations after pause and resume, want 1: the resume re-read a "+
			"manifest the pause should have kept", got)
	}
	d.mu.Lock()
	_, claimed := d.launched["a"]
	d.mu.Unlock()
	if !claimed {
		t.Error("the resumed job holds its lease but no Fetching worker was launched")
	}
}

// TestRestore_PausedJobIsNotHydratedUntilResumed pins that pause never loads a
// manifest: a paused job restored at startup holds nothing, so it stays on
// disk across ticks, and is hydrated only once a resume lets it take a lease.
func TestRestore_PausedJobIsNotHydratedUntilResumed(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{{
		ID:     "a",
		Header: Header{Name: "a"},
		State:  job.StateView{State: job.Fetching},
		Intent: job.IntentPause,
	}})
	res, hydrations := countingResidency()
	d := newTestDispatcher(t, withStore(st), withResidency(res))
	if err := d.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}

	d.tick(context.Background())
	d.tick(context.Background())
	if got := hydrations.Load(); got != 0 || res.resident("a") {
		t.Fatalf("%d hydrations of a paused restored job, resident = %v, want none", got, res.resident("a"))
	}

	if err := d.ResumeJob("a"); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	d.tick(context.Background())
	if got := hydrations.Load(); got != 1 || !res.resident("a") {
		t.Errorf("%d hydrations after the resume, resident = %v, want one and resident", got, res.resident("a"))
	}
}

// TestRemove_PausedResidentJobIsEvicted pins that the residency a pause keeps
// does not outlive the job: removing a paused, resident job evicts its
// manifest and leaves no lease, row or bookkeeping behind.
func TestRemove_PausedResidentJobIsEvicted(t *testing.T) {
	res, _ := countingResidency()
	st := &fakeStore{}
	d := newTestDispatcher(t, withResidency(res), withStore(st), withCaps(1, 1))
	j := fetchingWithLease(t, d, "a")
	if err := d.PauseJob("a"); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	d.tick(context.Background())
	if !res.resident("a") {
		t.Fatal("fixture: the paused job is not resident, so its eviction is not tested")
	}

	// Bounded: a job still holding a lease and a worker claim would make
	// Remove wait on that worker forever.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Remove(ctx, "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if res.resident("a") || d.isResident("a") {
		t.Errorf("removed paused job resident = %v, recorded resident = %v, want neither",
			res.resident("a"), d.isResident("a"))
	}
	if j.HoldsLease() {
		t.Error("the removed job still holds a lease")
	}
	if _, ok := st.row("a"); ok || !st.deleted("a") {
		t.Error("the removed job's row was not deleted")
	}
	if d.Len() != 0 {
		t.Errorf("Len = %d after removing the only job, want 0", d.Len())
	}
	b := fetchingWithLease(t, d, "b")
	if !b.HoldsLease() {
		t.Error("the lease the removed job gave back is not available to the next job")
	}
}

// TestPause_QueueWidePauseKeepsLeases pins that the queue-wide pause is not
// the per-job one: it gates moves but takes no lease from a working job, and
// keeps its manifest.
func TestPause_QueueWidePauseKeepsLeases(t *testing.T) {
	res, _ := countingResidency()
	d := newTestDispatcher(t, withResidency(res))
	j := fetchingWithLease(t, d, "a")

	d.Pause()
	d.tick(context.Background())

	if !j.HoldsLease() {
		t.Error("a queue-wide pause took the lease of a working Fetching job")
	}
	if !res.resident("a") {
		t.Error("a queue-wide pause evicted a working job's manifest")
	}
}

// TestReconcileResidency_DoesNotRehydrateAPausedSlotHolder pins that a job
// parked by a residency fault while it holds a compute slot is not re-read on
// every tick. Its first hydration faults and parks it (a pause, as
// Application.Stall does); a pause keeps the slot, and the ticks after that
// must leave the job alone until it is resumed.
func TestReconcileResidency_DoesNotRehydrateAPausedSlotHolder(t *testing.T) {
	runner := &stateRunner{}
	res := &fakeResidency{}
	d := newTestDispatcher(t, withRunner(runner), withResidency(res))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if err := d.AdvanceFrom(j, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom: %v", err)
	}
	d.tick(context.Background())
	if v := d.q.Render(j); !v.Holds || v.State != job.Assessing {
		t.Fatalf("fixture: j1 does not hold a slot at Assessing: %+v", v)
	}

	// A restart's state: the slot holder has no manifest loaded, and reading
	// its files faults.
	res.Evict("j1")
	d.markNotResident("j1")
	var hydrates atomic.Int32
	res.mu.Lock()
	res.failOn = map[string]error{"j1": fmt.Errorf("verify: %w", ErrResidencyFault)}
	res.mu.Unlock()
	res.onHydrate = func(id string) {
		hydrates.Add(1)
		if err := d.PauseJob(id); err != nil {
			t.Errorf("PauseJob: %v", err)
		}
	}

	for range 20 {
		d.tick(context.Background())
	}
	if got := hydrates.Load(); got != 1 {
		t.Errorf("Hydrate ran %d times over 20 ticks for a paused slot holder, want 1", got)
	}
	if v := d.q.Render(j); !v.Holds {
		t.Errorf("the paused job lost its slot: %+v", v)
	}
}
