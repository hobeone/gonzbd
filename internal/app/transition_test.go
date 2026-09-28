package app

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

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
