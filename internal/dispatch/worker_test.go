package dispatch

import (
	"context"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestCancelledWorker_SettlesRatherThanReAbortingForever is the most
// important test in this plan. Without the dispatcher's exit path, a
// cancelled worker's Abort never returns any resource: HoldsLease stays
// true, running() stays true, and every subsequent tick routes IntentCancel
// back to finishCancel, which calls Abort again and returns nil — the job
// never settles and holds pool-A capacity for the life of the process.
func TestCancelledWorker_SettlesRatherThanReAbortingForever(t *testing.T) {
	w := &stubWorkers{}
	d := newTestDispatcher(t, withWorkers(w))
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
	if len(w.aborted) != 1 {
		t.Fatalf("Abort called %d times, want 1", len(w.aborted))
	}

	// The worker notices the abort and exits without finishing.
	if err := d.Yielded(j); err != nil {
		t.Fatalf("Yielded: %v", err)
	}
	d.tick(context.Background())

	if got := d.q.Render(j).Outcome; got != job.OutcomeCancelled {
		t.Errorf("Outcome = %v, want Cancelled — without an exit path the job holds its lease, running() stays true, and finishCancel re-Aborts every tick forever", got)
	}
	if len(w.aborted) != 1 {
		t.Errorf("Abort called %d times in total, want 1 — a second call means the job never settled and the loop is live", len(w.aborted))
	}
}

func TestYielded_UnderPauseReturnsTheLease(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !j.HoldsLease() {
		t.Fatal("setup: job holds no lease")
	}

	d.Pause()
	if err := d.Yielded(j); err != nil {
		t.Fatalf("Yielded: %v", err)
	}

	if j.HoldsLease() {
		t.Error("lease still held after a pause yield — Advance branch 2 returns early while holds() is true, so only the dispatcher's exit path can return it")
	}
}

// TestFinished_SucceedsForARunningJob pins Finished's success path: a job
// with an open attempt settles and its resources are released.
func TestFinished_SucceedsForARunningJob(t *testing.T) {
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

	if err := d.Finished(j, job.OutcomeFailed); err != nil {
		t.Fatalf("Finished: %v", err)
	}
	if got := d.q.Render(j).Outcome; got != job.OutcomeFailed {
		t.Errorf("Outcome = %v, want OutcomeFailed", got)
	}
}

// TestFinished_PropagatesASettleError pins Finished's error-wrapping branch:
// a job with no open attempt cannot be settled, and Settle's refusal must
// come back through Finished rather than being swallowed.
func TestFinished_PropagatesASettleError(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// No tick: the job never opened an attempt.

	if err := d.Finished(j, job.OutcomeOK); err == nil {
		t.Fatal("Finished on a job with no open attempt returned nil, want an error")
	}
}

func TestFinished_RefusesCancelledAsAnOutcome(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())

	if err := d.Finished(j, job.OutcomeCancelled); err == nil {
		t.Fatal("Finished accepted OutcomeCancelled, want an error — only the cancel latch may produce it, and a worker reporting it would let any exit masquerade as a user deletion")
	}
}

// TestClaimLaunched_ClaimsOnce pins claimLaunched's contract directly: the
// first call to claim an ID succeeds, a second call before clearLaunched
// fails, and clearLaunched makes the ID claimable again.
func TestClaimLaunched_ClaimsOnce(t *testing.T) {
	d := newTestDispatcher(t)
	if !d.claimLaunched("j1") {
		t.Fatal("first claim should succeed")
	}
	if d.claimLaunched("j1") {
		t.Fatal("second claim before clear should fail")
	}
	d.clearLaunched("j1")
	if !d.claimLaunched("j1") {
		t.Fatal("claim after clearLaunched should succeed")
	}
}

// TestLaunch_DirectCallStartsWhenRunningAndClaimable calls launch directly
// (rather than through tick) to pin its two guards independently: it starts
// the runner only for a job that is Running and not already claimed.
func TestLaunch_DirectCallStartsWhenRunningAndClaimable(t *testing.T) {
	runner := &fakeRunner{}
	d := newTestDispatcher(t, withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !d.q.Render(j).Running {
		t.Fatal("setup: job is not running")
	}

	// The tick's own launch already claimed the job; clear it so this direct
	// call is the one that observes claimLaunched return true.
	d.clearLaunched(j.ID())
	runner.mu.Lock()
	runner.seen = map[string]bool{}
	runner.mu.Unlock()

	d.launch(context.Background(), j)

	if !runner.started(j.ID()) {
		t.Error("launch did not start the runner for a running, claimable job")
	}
}

// TestLaunch_DirectCallSkipsWhenNotRunning pins launch's Running guard: a job
// that has never ticked holds nothing and is not Running, so a direct launch
// call must not start it.
func TestLaunch_DirectCallSkipsWhenNotRunning(t *testing.T) {
	runner := &fakeRunner{}
	d := newTestDispatcher(t, withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.launch(context.Background(), j)

	if runner.started(j.ID()) {
		t.Error("launch started the runner for a job that is not Running")
	}
}

func TestLaunch_SkippedWhenIntentTurnedToCancelDuringHydration(t *testing.T) {
	runner := &fakeRunner{}
	res := &fakeResidency{}
	d := newTestDispatcher(t, withResidency(res), withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Cancel lands while the manifest read is in flight.
	res.onHydrate = func(string) { _ = d.Cancel(j.ID()) }

	d.tick(context.Background())
	d.tick(context.Background())

	if runner.started(j.ID()) {
		t.Error("worker launched for a job cancelled during hydration — the launch path must re-read the snapshot after the unlocked read")
	}
}
