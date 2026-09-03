package postproc

import (
	"testing"
	"time"
)

// TestPPQueueNotify_Coalesces pins notify's two documented properties: it
// sends to the cap-1 notifyCh non-blockingly, so repeated calls while a
// wakeup is already pending coalesce into that single pending value rather
// than blocking or queueing a second one.
func TestPPQueueNotify_Coalesces(t *testing.T) {
	q := newPPQueue()

	q.notify()
	select {
	case <-q.notifyCh:
	default:
		t.Fatal("notify did not leave a pending wakeup on an unarmed notifyCh")
	}

	// Re-arm, then call notify twice more without draining. If notify
	// blocked once a wakeup was already pending, this goroutine would never
	// close done and the select below would time out.
	q.notify()
	done := make(chan struct{})
	go func() {
		q.notify()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify blocked while a wakeup was already pending -- it must be non-blocking")
	}

	select {
	case <-q.notifyCh:
	default:
		t.Fatal("notify left no pending wakeup after coalescing repeated calls")
	}
	select {
	case <-q.notifyCh:
		t.Fatal("notify queued a second wakeup instead of coalescing into the first")
	default:
		// Expected: nothing further pending.
	}
}

// TestFindJob pins findJob's contract directly: the index of the job with
// the given ID, including a non-first position, -1 when absent, and -1 on
// an empty (nil) slice.
func TestFindJob(t *testing.T) {
	if idx := findJob(nil, "x"); idx != -1 {
		t.Errorf("findJob(nil, \"x\") = %d, want -1", idx)
	}

	jobs := []*Job{
		{Queue: newQueueJob(t, "a", 0)},
		{Queue: newQueueJob(t, "b", 0)},
		{Queue: newQueueJob(t, "c", 0)},
	}

	if idx := findJob(jobs, "b"); idx != 1 {
		t.Errorf("findJob(jobs, \"b\") = %d, want 1 (non-first position)", idx)
	}
	if idx := findJob(jobs, "missing"); idx != -1 {
		t.Errorf("findJob(jobs, \"missing\") = %d, want -1", idx)
	}
}

// TestTryPop pins tryPop's contract directly: nil on an empty queue, FIFO
// order on a non-empty one (verified rather than assumed), the mark
// callback receiving the popped job while q.mu is held, and tolerance of a
// nil mark.
func TestTryPop(t *testing.T) {
	q := newPPQueue()

	if job := q.tryPop(nil); job != nil {
		t.Fatalf("tryPop on an empty queue = %v, want nil", job)
	}

	jobA := &Job{Queue: newQueueJob(t, "a", 0)}
	jobB := &Job{Queue: newQueueJob(t, "b", 0)}
	q.Push(jobA)
	q.Push(jobB)

	var marked *Job
	got := q.tryPop(func(j *Job) { marked = j })
	if got != jobA {
		t.Fatalf("tryPop returned %v, want the front job %v (FIFO)", got, jobA)
	}
	if marked != jobA {
		t.Fatalf("mark received %v, want %v", marked, jobA)
	}
	if n := q.Len(); n != 1 {
		t.Fatalf("Len() = %d after tryPop, want 1", n)
	}

	// nil mark must not panic, and the second, previously-back job must now
	// come out front (still FIFO).
	got2 := q.tryPop(nil)
	if got2 != jobB {
		t.Fatalf("tryPop returned %v, want %v", got2, jobB)
	}
	if n := q.Len(); n != 0 {
		t.Fatalf("Len() = %d after draining the queue, want 0", n)
	}
}

// TestWithLock pins withLock's contract directly: fn receives the live
// jobs slice, and q.mu is genuinely held for fn's whole duration -- proved
// by having a concurrent Len() call block until fn returns, not merely by
// asserting the values fn saw.
func TestWithLock(t *testing.T) {
	q := newPPQueue()
	q.Push(&Job{Queue: newQueueJob(t, "a", 0)})

	var gotJobs []*Job
	entered := make(chan struct{})
	proceed := make(chan struct{})

	go func() {
		q.withLock(func(jobs []*Job) {
			gotJobs = jobs
			close(entered)
			<-proceed
		})
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("withLock never invoked fn")
	}

	lenDone := make(chan int, 1)
	go func() { lenDone <- q.Len() }()

	select {
	case v := <-lenDone:
		t.Fatalf("Len() returned %d while withLock's fn was still running -- q.mu was not held for fn's duration", v)
	case <-time.After(100 * time.Millisecond):
		// Expected: Len() is blocked on q.mu.
	}

	close(proceed)

	select {
	case v := <-lenDone:
		if v != 1 {
			t.Errorf("Len() = %d after withLock released q.mu, want 1", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Len() never returned after withLock released q.mu")
	}

	if len(gotJobs) != 1 || gotJobs[0].Queue.ID != "a" {
		t.Fatalf("withLock passed jobs = %v, want the single pushed job with ID \"a\"", gotJobs)
	}
}
