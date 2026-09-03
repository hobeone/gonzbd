package postproc

import (
	"context"
	"sync"
)

// ppQueue is the scheduling primitive used by PostProcessor.
// There is exactly one consumer (the PostProcessor worker goroutine) and
// potentially many producers (Process callers).
//
// The notifyCh (capacity 1) is signalled on every push; Pop blocks on it
// when the queue is empty.
type ppQueue struct {
	mu       sync.Mutex
	jobs     []*Job
	notifyCh chan struct{}
}

// newPPQueue constructs an empty ppQueue.
func newPPQueue() *ppQueue {
	return &ppQueue{
		notifyCh: make(chan struct{}, 1),
	}
}

// Push enqueues a job onto the queue.
func (q *ppQueue) Push(job *Job) {
	q.mu.Lock()
	q.jobs = append(q.jobs, job)
	q.mu.Unlock()
	q.notify()
}

// notify sends to notifyCh non-blockingly; the cap-1 channel coalesces
// multiple rapid pushes into a single wakeup.
func (q *ppQueue) notify() {
	select {
	case q.notifyCh <- struct{}{}:
	default:
	}
}

// Len returns the queue length without locking for long — safe to call
// from the consumer goroutine or from tests.
func (q *ppQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

// Empty returns true when the queue is empty.
func (q *ppQueue) Empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs) == 0
}

// Cancel removes a job with the given ID from the queue.
// Returns true if the job was found and removed.
func (q *ppQueue) Cancel(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if idx := findJob(q.jobs, jobID); idx >= 0 {
		copy(q.jobs[idx:], q.jobs[idx+1:])
		q.jobs[len(q.jobs)-1] = nil // allow GC
		q.jobs = q.jobs[:len(q.jobs)-1]
		return true
	}
	return false
}

// Has reports whether a job with the given ID is currently queued.
// Does not inspect the in-flight job (if any); callers that need to
// know about the active job should check that separately.
func (q *ppQueue) Has(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return findJob(q.jobs, jobID) >= 0
}

// findJob returns the index of the job with the given ID, or -1.
func findJob(jobs []*Job, id string) int {
	for i, j := range jobs {
		if j.JobID() == id {
			return i
		}
	}
	return -1
}

// Pop blocks until a job is available or ctx is done.
// Returns the next job and true, or nil and false when ctx is cancelled.
//
// mark, when non-nil, is invoked on the popped job while q.mu is still held
// (see tryPop) -- i.e. before Pop's caller can observe the job as removed
// from the queue via any other method on q. This lets a caller publish
// "now in flight" state atomically with the removal, closing the window
// where the job would otherwise be observable in neither the queue nor the
// caller's own busy state. mark must not call back into q (it already holds
// q.mu) and must return promptly, since it runs under the lock.
//
// Must be called from exactly one goroutine (the worker).
func (q *ppQueue) Pop(ctx context.Context, mark func(*Job)) (*Job, bool) {
	for {
		// Try to dequeue without waiting.
		if job := q.tryPop(mark); job != nil {
			return job, true
		}

		// Queue empty — wait for a push notification or ctx done.
		select {
		case <-ctx.Done():
			return nil, false
		case <-q.notifyCh:
			// Re-arm: there may be more items than the single notification.
			// Loop back and try again.
		}
	}
}

// tryPop dequeues one job, or returns nil. If mark is non-nil, it runs on
// the popped job before q.mu is released -- see Pop's doc comment.
func (q *ppQueue) tryPop(mark func(*Job)) *Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.jobs) == 0 {
		return nil
	}

	job := q.jobs[0]
	q.jobs[0] = nil // allow GC of old pointer
	q.jobs = q.jobs[1:]
	if mark != nil {
		mark(job)
	}
	return job
}

// withLock runs fn while holding q.mu, passing it the live q.jobs slice. It
// exists so PostProcessor.Has and PostProcessor.Empty can read the queue and
// the busyMu-guarded fields under one consistent q.mu -> busyMu lock order
// (the same order tryPop's mark callback uses to publish busy state),
// instead of taking and releasing each lock independently -- which is what
// let the two reads tear and made Has/Empty observe the job as absent from
// both.
//
// Passing jobs as a parameter rather than letting fn reach into q.jobs
// directly keeps the lock-held slice scoped to the callback: there is no
// longer a second, unexported-field access path that a future caller could
// use to read q.jobs outside q.mu without a compile error forcing the
// question. fn must not retain or mutate jobs beyond its own call, since the
// slice header aliases the live backing array under lock.
func (q *ppQueue) withLock(fn func(jobs []*Job)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	fn(q.jobs)
}
