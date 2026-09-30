package app

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/hobeone/gonzbd/internal/job"
)

func TestJobTransitions_TryAcquireTakesTheFreeIDsAndSkipsTheHeld(t *testing.T) {
	t.Parallel()
	var tr jobTransitions

	held := tr.tryAcquire("b")
	if !held.holds("b") {
		t.Fatal("tryAcquire of a free id did not take it")
	}

	c := tr.tryAcquire("a", "b", "c")
	if !c.holds("a") || !c.holds("c") {
		t.Errorf("claim = %v, want a and c taken", c.ids)
	}
	if c.holds("b") {
		t.Error("tryAcquire took b while another claim holds it")
	}
	c.release()

	held.release()
	again := tr.tryAcquire("b")
	if !again.holds("b") {
		t.Error("b was not free after its holder released it")
	}
	again.release()
}

// TestJobTransitions_FinalizingRecordIsCountedAndSkippedByTryAcquire: an ID
// stays recorded until every finalizer of it has ended, tryAcquire skips it
// meanwhile, and acquire, which a finalizer and a removal use, does not.
func TestJobTransitions_FinalizingRecordIsCountedAndSkippedByTryAcquire(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	end1 := tr.beginFinalize("a")
	end2 := tr.beginFinalize("a")
	if !tr.isFinalizing("a") || tr.isFinalizing("b") {
		t.Fatalf("isFinalizing(a, b) = (%v, %v), want (true, false)", tr.isFinalizing("a"), tr.isFinalizing("b"))
	}
	c := tr.tryAcquire("a", "b")
	if c.holds("a") || !c.holds("b") {
		t.Errorf("claim = %v, want b taken and a skipped", c.ids)
	}
	c.release()
	w, err := tr.acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("acquire(a) while it is recorded: %v", err)
	}
	w.release()

	end1()
	if !tr.isFinalizing("a") {
		t.Error("a unrecorded while a second finalizer of it runs")
	}
	end2()
	if tr.isFinalizing("a") {
		t.Error("a still recorded after every finalizer of it ended")
	}
	again := tr.tryAcquire("a")
	if !again.holds("a") {
		t.Error("tryAcquire skipped a after its finalizers ended")
	}
	again.release()
}

// TestJobTransitions_ClaimHelpers: claimLocked takes a free ID once, and
// claimOrHolder claims a free ID or hands back the holder's channel, which
// closes when the holder releases.
func TestJobTransitions_ClaimHelpers(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	tr.mu.Lock()
	first, second := tr.claimLocked("a"), tr.claimLocked("a")
	tr.mu.Unlock()
	if !first || second {
		t.Errorf("claimLocked(a) twice = (%v, %v), want (true, false)", first, second)
	}
	holder := tr.claimOrHolder("a")
	if holder == nil {
		t.Fatal("claimOrHolder(a) claimed an ID another claim holds")
	}
	if ch := tr.claimOrHolder("b"); ch != nil {
		t.Error("claimOrHolder(b) did not claim a free ID")
	}
	(&transitionClaim{t: &tr, ids: map[string]struct{}{"a": {}}}).release()
	select {
	case <-holder:
	default:
		t.Error("the holder's channel stayed open after it released")
	}
}

// TestJobTransitions_RemovedMarkForgotten: forgetRemoved, the collection
// cleanup, drops a mark.
func TestJobTransitions_RemovedMarkForgotten(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	j := job.New("a", "a", job.Policy{})
	tr.markRemoved(j)
	tr.forgetRemoved(weak.Make(j))
	if tr.wasRemoved(j) {
		t.Error("the mark stayed after forgetRemoved")
	}
}

func TestJobTransitions_AcquireWaitsForTheHolder(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	held := tr.tryAcquire("x")

	got := make(chan *transitionClaim, 1)
	go func() {
		c, err := tr.acquire(context.Background(), "x")
		if err != nil {
			t.Errorf("acquire: %v", err)
		}
		got <- c
	}()

	select {
	case <-got:
		t.Fatal("acquire returned while another claim held the id")
	case <-time.After(50 * time.Millisecond):
	}
	held.release()

	select {
	case c := <-got:
		if !c.holds("x") {
			t.Error("acquire returned a claim that does not hold x")
		}
		c.release()
	case <-time.After(5 * time.Second):
		t.Fatal("acquire did not proceed after the holder released")
	}
}

func TestJobTransitions_AcquireGivesUpWithItsContext(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	held := tr.tryAcquire("x")
	defer held.release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	c, err := tr.acquire(ctx, "x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire err = %v, want DeadlineExceeded", err)
	}
	if c != nil {
		t.Fatal("acquire returned a claim alongside its error")
	}
}

// An ended context does not stop acquire taking a free id: a finalizer on a
// stopping process still takes a lock no one holds.
func TestJobTransitions_AcquireTakesAFreeIDOnAnEndedContext(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, err := tr.acquire(ctx, "x")
	if err != nil {
		t.Fatalf("acquire err = %v on a free id, want a claim", err)
	}
	if !c.holds("x") {
		t.Fatal("the claim does not hold the free id")
	}
	c.release()
}

// A waiter woken by its holder's release must re-check before taking the id:
// here the release and a second claim of the id happen in one critical
// section, so by the time the waiter runs, the id is held again.
func TestJobTransitions_AWokenWaiterRechecksBeforeTakingTheID(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	tr.tryAcquire("x")

	got := make(chan *transitionClaim, 1)
	go func() {
		c, err := tr.acquire(context.Background(), "x")
		if err != nil {
			t.Errorf("acquire: %v", err)
		}
		got <- c
	}()
	select {
	case <-got:
		t.Fatal("acquire returned while another claim held the id")
	case <-time.After(150 * time.Millisecond):
	}

	tr.mu.Lock()
	close(tr.held["x"])
	tr.held["x"] = make(chan struct{})
	tr.mu.Unlock()
	second := &transitionClaim{t: &tr, ids: map[string]struct{}{"x": {}}}

	select {
	case <-got:
		t.Fatal("a woken waiter took the id while a second claim held it")
	case <-time.After(150 * time.Millisecond):
	}
	second.release()
	select {
	case c := <-got:
		if !c.holds("x") {
			t.Error("acquire returned a claim that does not hold x")
		}
		c.release()
	case <-time.After(5 * time.Second):
		t.Fatal("acquire did not proceed after the second claim released")
	}
}

// Several waiters on one id: exactly one holds it at a time, and every one of
// them gets it in the end. A waiter that took the holder's channel before the
// release must re-check rather than assume it won.
func TestJobTransitions_SeveralWaitersTakeTheIDOneAtATime(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	held := tr.tryAcquire("x")

	const waiters = 8
	inside := make(chan struct{}, waiters)
	done := make(chan struct{}, waiters)
	var holding atomic.Int32
	for range waiters {
		go func() {
			c, err := tr.acquire(context.Background(), "x")
			if err != nil {
				t.Errorf("acquire: %v", err)
				done <- struct{}{}
				return
			}
			if n := holding.Add(1); n != 1 {
				t.Errorf("%d claims hold x at once", n)
			}
			inside <- struct{}{}
			holding.Add(-1)
			c.release()
			done <- struct{}{}
		}()
	}
	held.release()

	deadline := time.After(5 * time.Second)
	for range waiters {
		select {
		case <-done:
		case <-deadline:
			t.Fatal("not every waiter got the id")
		}
	}
	if len(inside) != waiters {
		t.Errorf("%d waiters held x, want %d", len(inside), waiters)
	}
}

func TestJobTransitions_ReleaseRemovesTheIDs(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	c := tr.tryAcquire("a", "b")
	c.release()
	tr.mu.Lock()
	n := len(tr.held)
	tr.mu.Unlock()
	if n != 0 {
		t.Errorf("%d ids still in the map after release, want 0: it would grow with every job ever touched", n)
	}
}

func TestJobTransitions_ASecondReleasePanics(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	c := tr.tryAcquire("a")
	c.release()
	defer func() {
		if recover() == nil {
			t.Error("a second release did not panic")
		}
	}()
	c.release()
}

// A removal mark belongs to the job instance it was made on, not its ID: a
// later job under the same ID is not affected.
func TestJobTransitions_RemovedMarksTheInstance(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	removed := job.New("x", "x", job.Policy{})
	later := job.New("x", "x", job.Policy{})
	if tr.wasRemoved(removed) {
		t.Fatal("an unmarked job reads as removed")
	}
	tr.markRemoved(removed)
	tr.markRemoved(removed)
	if !tr.wasRemoved(removed) {
		t.Error("a marked job does not read as removed")
	}
	if tr.wasRemoved(later) {
		t.Error("a later job under the same ID reads as removed")
	}
}

// The record does not outlive the job: once nothing references a marked job,
// its entry goes, so the record does not grow with every job ever removed.
func TestJobTransitions_RemovedForgetsACollectedJob(t *testing.T) {
	t.Parallel()
	var tr jobTransitions
	tr.markRemoved(job.New("gone", "gone", job.Policy{}))

	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		tr.mu.Lock()
		n := len(tr.removed)
		tr.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d removal marks remain after the job was collected", n)
		}
		runtime.Gosched()
	}
}
