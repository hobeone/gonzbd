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

// PushHead prepends a job to the front of the queue. Used by popWithPause
// to unpop a job when Pause is observed after Pop has already returned
// it — without this, a job popped just before the pause check would leak
// past the pause. No notify: the worker that's putting the job back is
// about to go wait on resumeC, not on notifyCh.
func (q *ppQueue) PushHead(job *Job) {
	q.mu.Lock()
	q.jobs = append([]*Job{job}, q.jobs...)
	q.mu.Unlock()
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
		q.jobs = append(q.jobs[:idx], q.jobs[idx+1:]...)
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
		if j.Queue.ID == id {
			return i
		}
	}
	return -1
}

// Pop blocks until a job is available or ctx is done.
// Returns the next job and true, or nil and false when ctx is cancelled.
//
// Must be called from exactly one goroutine (the worker).
func (q *ppQueue) Pop(ctx context.Context) (*Job, bool) {
	for {
		// Try to dequeue without waiting.
		if job := q.tryPop(); job != nil {
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

// tryPop dequeues one job, or returns nil.
func (q *ppQueue) tryPop() *Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.jobs) == 0 {
		return nil
	}

	job := q.jobs[0]
	q.jobs = q.jobs[1:]
	return job
}
