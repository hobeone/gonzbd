package checkpoint

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestMark_RefusesAnInstanceItHasPruned: a mark arriving after the instance's
// Prune is dropped, so no later flush writes the departed run's state.
func TestMark_RefusesAnInstanceItHasPruned(t *testing.T) {
	t.Parallel()
	st := &recordingStore{}
	c := New(st, time.Hour, nil)
	old := job.New("a", "A", job.PolicyFromPP(3))

	c.Mark(old)
	c.Prune(old)
	c.Mark(old)

	if got := c.DirtyCount(); got != 0 {
		t.Fatalf("DirtyCount = %d, want 0: a mark of a pruned instance was kept", got)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(st.batches) != 0 {
		t.Fatalf("a flush wrote %d batches for a pruned instance, want 0", len(st.batches))
	}
}

// TestMark_AcceptsALaterInstanceUnderAPrunedID: the refusal is by instance, so a
// retry registered under the departed job's ID checkpoints normally, and a
// stale mark of the departed instance cannot displace the retry's pending one.
func TestMark_AcceptsALaterInstanceUnderAPrunedID(t *testing.T) {
	t.Parallel()
	st := &recordingStore{}
	c := New(st, time.Hour, nil)
	old := job.New("a", "Old", job.PolicyFromPP(3))
	retry := job.New("a", "Retry", job.PolicyFromPP(3))
	if err := retry.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}

	c.Prune(old)
	c.Mark(retry)
	c.Mark(old) // late, after the retry's mark

	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(st.batches) != 1 || len(st.batches[0]) != 1 {
		t.Fatalf("batches = %v, want one batch holding the retry", st.batches)
	}
	if got := st.batches[0][0].Intent; got != job.IntentPause {
		t.Fatalf("flushed intent = %v, want the retry's %v: the pruned instance's "+
			"mark displaced the retry's", got, job.IntentPause)
	}
}

// TestUnprune_RestoresMarking: a departure that gave up hands the instance
// back, and its next mark is written.
func TestUnprune_RestoresMarking(t *testing.T) {
	t.Parallel()
	c := New(&recordingStore{}, time.Hour, nil)
	j := job.New("a", "A", job.PolicyFromPP(3))

	c.Prune(j)
	c.Unprune(j)
	c.Mark(j)

	if got := c.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount = %d, want 1: an unpruned instance's mark was refused", got)
	}
	c.mu.Lock()
	n := len(c.pruned)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d refusals remain after Unprune, want 0", n)
	}
}

// TestForgetPruned_DropsOnlyThatInstance: the cleanup the runtime runs for a
// collected instance removes that instance's refusal and no other.
func TestForgetPruned_DropsOnlyThatInstance(t *testing.T) {
	t.Parallel()
	c := New(&recordingStore{}, time.Hour, nil)
	a := job.New("a", "A", job.PolicyFromPP(3))
	b := job.New("b", "B", job.PolicyFromPP(3))
	c.Prune(a)
	c.Prune(b)

	c.forgetPruned(weak.Make(a))

	c.mu.Lock()
	_, aKept := c.pruned[weak.Make(a)]
	_, bKept := c.pruned[weak.Make(b)]
	c.mu.Unlock()
	if aKept || !bKept {
		t.Fatalf("after forgetting a: a refused = %v, b refused = %v; want false, true", aKept, bKept)
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
}

// TestUnprune_WithdrawsOnlyItsOwnPrune: two departures can prune one instance —
// a RemoveJob and a finalizer that proceeded without the transition lock — and
// the RemoveJob's Unprune after a failed queue removal must not withdraw the
// refusal the finalizer's prune still needs once its own removal succeeded.
func TestUnprune_WithdrawsOnlyItsOwnPrune(t *testing.T) {
	t.Parallel()
	c := New(&recordingStore{}, time.Hour, nil)
	j := job.New("a", "A", job.PolicyFromPP(3))

	c.Prune(j) // RemoveJob
	c.Prune(j) // the finalizer
	c.Unprune(j)
	c.Mark(j)

	if got := c.DirtyCount(); got != 0 {
		t.Fatalf("DirtyCount = %d, want 0: one Unprune withdrew both departures' "+
			"refusals, so a late mark of an instance the finalizer removed is kept", got)
	}

	c.Unprune(j)
	c.Mark(j)
	if got := c.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount = %d, want 1 once every prune is withdrawn", got)
	}
}

// TestPrune_LeavesALaterInstancesPendingMark: pruning a departed instance must
// not drop the pending mark of a later instance under the same ID.
func TestPrune_LeavesALaterInstancesPendingMark(t *testing.T) {
	t.Parallel()
	c := New(&recordingStore{}, time.Hour, nil)
	old := job.New("a", "Old", job.PolicyFromPP(3))
	retry := job.New("a", "Retry", job.PolicyFromPP(3))

	c.Mark(retry)
	c.Prune(old)

	if got := c.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount = %d, want 1: pruning the departed instance dropped "+
			"the retry's pending mark", got)
	}
}

// waitUntilPruned blocks until j is in the refusal set, which Prune records
// under the same lock acquisition as its deletions.
func waitUntilPruned(t *testing.T, c *Checkpointer, j *job.Job) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		_, ok := c.pruned[weak.Make(j)]
		c.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Prune never recorded the instance")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPrune_LeavesALaterInstanceInFlight: pruning a departed instance while a
// failing flush carries a later instance under the same ID must not stop that
// flush re-merging the later instance.
func TestPrune_LeavesALaterInstanceInFlight(t *testing.T) {
	t.Parallel()
	st := &failingStore{failsLeft: 1}
	c := New(st, time.Hour, nil)
	old := job.New("a", "Old", job.PolicyFromPP(3))
	retry := job.New("a", "Retry", job.PolicyFromPP(3))
	c.Mark(retry)

	pruned := make(chan struct{})
	st.beforeFail = func() {
		go func() {
			c.Prune(old)
			close(pruned)
		}()
		waitUntilPruned(t, c, old)
	}
	if err := c.Flush(context.Background()); !errors.Is(err, errSaveBatchFailed) {
		t.Fatalf("Flush: got %v, want errSaveBatchFailed", err)
	}
	<-pruned

	if got := c.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount = %d, want 1: pruning the departed instance took the "+
			"retry out of the failed flush's re-merge", got)
	}
}

// TestPrune_ForgetsACollectedInstance: the refusal does not outlive the job, so
// the record does not grow with every job ever pruned.
func TestPrune_ForgetsACollectedInstance(t *testing.T) {
	t.Parallel()
	c := New(&recordingStore{}, time.Hour, nil)
	c.Prune(job.New("gone", "gone", job.PolicyFromPP(3)))

	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		c.mu.Lock()
		n := len(c.pruned)
		c.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d refusals remain after the pruned job was collected", n)
		}
		runtime.Gosched()
	}
}
