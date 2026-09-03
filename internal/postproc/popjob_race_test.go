package postproc

import (
	"context"
	"testing"
	"time"
)

// TestPopJob_HasAndEmptyBlockDuringTransition is the regression test for the
// popJob window described in popJob's doc comment: a job popped from the
// queue must never be observable, even momentarily, as absent from BOTH the
// queue (ppQueue.Has) and the busy state (PostProcessor.busy /
// currentJobID). Before the fix, p.q.Pop(p.workerCtx) released q.mu and
// returned the job to popJob's caller before p.setBusyWithJob ran, so a
// concurrent Has/Empty call landing in that window saw the job nowhere.
//
// The real window is a handful of nanoseconds inside a single function
// call, entirely too small to reproduce by racing free-running goroutines
// against each other -- an earlier version of this test tried exactly that
// (many push/popJob cycles against several spinning Has/Empty observers)
// and produced thousands of false failures against the ALREADY-FIXED
// implementation, because on a 32-core machine the unsynchronized producer
// simply outran the observers between their own read of "which job is
// live" and their subsequent Has/Empty call -- a scheduling artifact of
// the test, not of popJob.
//
// This test controls the transition directly instead of racing it. It
// holds p.busyMu itself before starting popJob in a goroutine, so
// setBusyWithJob (called from popJob's mark closure, which tryPop invokes
// while still holding q.mu -- see ppQueue.tryPop / ppQueue.Pop) blocks
// trying to acquire busyMu. Because q.mu is not released until mark
// returns, q.mu therefore stays held for the WHOLE transition on the fixed
// code, verified with waitUntil's poll-with-deadline (the standard
// synchronization idiom already used throughout this package -- see
// startProcessor's callers). With q.mu held, Has and Empty -- which both
// start with p.q.withLock -- cannot get past their own q.mu.Lock() to
// answer at all, since sync.Mutex is not reentrant; they must block. This
// makes the property deterministic: it follows from mutual exclusion, not
// from winning a scheduling race.
//
// On the unfixed popJob (q.Pop with no mark, followed by a separate
// setBusyWithJob call after Pop has already returned), tryPop releases
// q.mu as soon as the job is removed from the slice -- well before
// touching busyMu -- so q.mu goes back to free almost immediately and the
// waitUntil below times out, failing the test.
func TestPopJob_HasAndEmptyBlockDuringTransition(t *testing.T) {
	t.Parallel()

	p := New(Options{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.workerCtx = ctx

	const jobID = "torn-check"
	p.q.Push(&Job{Job: newQueueJob(t, jobID, 0)})

	// Block setBusyWithJob's busyMu.Lock() so the transition halts
	// mid-flight, under our control.
	p.busyMu.Lock()

	type popResult struct {
		job    *Job
		jobCtx context.Context
		ok     bool
	}
	popDone := make(chan popResult, 1)
	go func() {
		job, jobCtx, ok := p.popJob()
		popDone <- popResult{job, jobCtx, ok}
	}()

	waitUntil(t, func() bool {
		if p.q.mu.TryLock() {
			p.q.mu.Unlock()
			return false
		}
		return true
	}, 2*time.Second, "ppQueue.tryPop to be holding q.mu while mark blocks on busyMu")

	// q.mu is held. On the fixed code the job is already removed from the
	// slice (tryPop does that before invoking mark) and busy/currentJobID
	// are not yet published (mark's setBusyWithJob call is the thing
	// blocked on busyMu right now) -- exactly the torn state Has/Empty must
	// never answer from. Prove they don't: launch both and confirm neither
	// returns while q.mu stays held.
	hasDone := make(chan bool, 1)
	emptyDone := make(chan bool, 1)
	go func() { hasDone <- p.Has(jobID) }()
	go func() { emptyDone <- p.Empty() }()

	select {
	case v := <-hasDone:
		t.Fatalf("Has(%q) returned %v while q.mu was held mid-transition -- it must block on q.mu, not answer from torn state", jobID, v)
	case <-time.After(100 * time.Millisecond):
		// Expected: still blocked.
	}
	select {
	case v := <-emptyDone:
		t.Fatalf("Empty() returned %v while q.mu was held mid-transition -- it must block on q.mu, not answer from torn state", v)
	case <-time.After(100 * time.Millisecond):
		// Expected: still blocked.
	}

	// Let the transition complete: mark finishes (publishing busy=true,
	// currentJobID=jobID), tryPop releases q.mu, popJob returns, and the
	// blocked Has/Empty calls can now proceed to a consistent answer.
	p.busyMu.Unlock()

	res := <-popDone
	if !res.ok {
		t.Fatal("popJob returned ok=false for a queue with one job pushed")
	}
	if res.job.JobID() != jobID {
		t.Fatalf("popJob returned job %q, want %q", res.job.JobID(), jobID)
	}

	select {
	case v := <-hasDone:
		if !v {
			t.Error("Has(jobID) = false once the transition completed, want true (job is now in flight)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Has never returned after q.mu/busyMu were released")
	}
	select {
	case v := <-emptyDone:
		if v {
			t.Error("Empty() = true once the transition completed, want false (a job is in flight)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Empty never returned after q.mu/busyMu were released")
	}
}

// TestPPQueueTryPop_MarkRunsWhileQueueLockHeld pins ppQueue's own half of
// the fix directly, independent of PostProcessor: tryPop must invoke its
// mark callback before releasing q.mu, not after. This is deterministic via
// a channel handshake rather than timing: mark blocks on <-proceed, so the
// close of entered is a happens-before guarantee that mark is genuinely
// still running (and therefore that tryPop has not yet reached its deferred
// q.mu.Unlock) at the moment this goroutine calls TryLock.
//
// TestPopJob_HasAndEmptyBlockDuringTransition above cannot catch a mutation
// that neuters the "if mark != nil { mark(job) }" call in tryPop on its
// own (with popJob's wiring left untouched): if mark never runs at all,
// popJob's setBusyWithJob call never happens either, so that test's
// controlled busyMu contention is never entered and its waitUntil would be
// racing the same nanosecond window it was built to avoid. This test
// targets ppQueue.tryPop's contract directly instead.
func TestPPQueueTryPop_MarkRunsWhileQueueLockHeld(t *testing.T) {
	t.Parallel()

	q := newPPQueue()
	q.Push(&Job{Job: newQueueJob(t, "x", 0)})

	entered := make(chan struct{})
	proceed := make(chan struct{})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	popDone := make(chan bool, 1)
	go func() {
		_, ok := q.Pop(ctx, func(*Job) {
			close(entered)
			<-proceed
		})
		popDone <- ok
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("mark was never invoked -- tryPop must call it for every popped job")
	}

	// mark is now blocked on <-proceed, guaranteed by the channel handshake
	// above. If tryPop released q.mu before calling mark, or called it
	// without holding q.mu at all, TryLock would succeed here.
	lockedElsewhere := q.mu.TryLock()
	if lockedElsewhere {
		q.mu.Unlock()
	}

	close(proceed)
	if !<-popDone {
		t.Fatal("Pop returned ok=false for a queue with one job pushed")
	}

	if lockedElsewhere {
		t.Fatal("q.mu was not held while mark was running -- tryPop must hold q.mu for the whole mark call")
	}
}

// TestHas_RequiresQueueLockEvenForCurrentJob pins Has's lock ORDER, which
// TestPopJob_HasAndEmptyBlockDuringTransition cannot: that test forces
// contention on busyMu, and the pre-fix Has (busyMu.Lock/Unlock, THEN
// separately p.q.Has, which itself takes q.mu) also blocks on busyMu in
// that scenario -- for the wrong reason, so it would survive as a mutant
// there. This test instead holds ONLY q.mu externally and marks a job
// busy directly (bypassing the queue), so the two implementations
// diverge: the pre-fix Has finds the match in its busyMu-only first step
// and returns immediately WITHOUT ever touching q.mu, while the fixed Has
// takes q.mu unconditionally as its outer lock (see ppQueue.withLock) and
// so must block for as long as q.mu is held, deterministically -- the test
// holds q.mu for a full 100ms itself, not racing anything.
func TestHas_RequiresQueueLockEvenForCurrentJob(t *testing.T) {
	t.Parallel()

	p := New(Options{})
	const jobID = "current-job"
	p.setBusyWithJob(true, jobID, nil)

	p.q.mu.Lock()

	done := make(chan bool, 1)
	go func() { done <- p.Has(jobID) }()

	select {
	case v := <-done:
		t.Fatalf("Has(%q) returned %v while q.mu was held externally, without Has ever needing q.mu for this jobID -- it must take q.mu unconditionally, even for the current job", jobID, v)
	case <-time.After(100 * time.Millisecond):
		// Expected: still blocked -- Has takes q.mu before checking anything.
	}

	p.q.mu.Unlock()

	select {
	case v := <-done:
		if !v {
			t.Error("Has(jobID) = false once q.mu was released, want true for the current job")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Has never returned after q.mu was released")
	}
}

// TestEmpty_RequiresQueueLockEvenWhenBusy is TestHas_RequiresQueueLockEvenForCurrentJob's
// counterpart for Empty. The pre-fix Empty is "!busy && p.q.Empty()": with
// busy already true, Go's && short-circuits and p.q.Empty() -- the only
// call that would touch q.mu -- is never evaluated, so the pre-fix version
// returns false immediately regardless of q.mu. The fixed Empty takes q.mu
// unconditionally as its outer lock, so it must block for as long as this
// test holds q.mu.
func TestEmpty_RequiresQueueLockEvenWhenBusy(t *testing.T) {
	t.Parallel()

	p := New(Options{})
	p.setBusyWithJob(true, "x", nil)

	p.q.mu.Lock()

	done := make(chan bool, 1)
	go func() { done <- p.Empty() }()

	select {
	case v := <-done:
		t.Fatalf("Empty() returned %v while q.mu was held externally, without Empty ever needing q.mu -- it must take q.mu unconditionally, even when busy is already known true", v)
	case <-time.After(100 * time.Millisecond):
		// Expected: still blocked -- Empty takes q.mu before checking busy.
	}

	p.q.mu.Unlock()

	select {
	case v := <-done:
		if v {
			t.Error("Empty() = true once q.mu was released, want false while busy")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Empty never returned after q.mu was released")
	}
}
