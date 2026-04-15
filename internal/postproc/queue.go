package postproc

import (
	"context"
	"sync"
)

// ppQueue is the fast/slow scheduling primitive used by PostProcessor.
// There is exactly one consumer (the PostProcessor worker goroutine) and
// potentially many producers (Process callers).
//
// Scheduling rule (mirrors Python MAX_FAST_JOB_COUNT logic exactly):
//
//	On Pop, if fastInRow >= maxFastPerCycle AND slow queue is non-empty
//	→ pull from slow, reset fastInRow.
//	Otherwise, prefer fast (increment fastInRow);
//	if fast is empty, fall back to slow (reset fastInRow).
//
// The notifyCh (capacity 1) is signalled on every push; Pop blocks on it
// when both queues are empty.
type ppQueue struct {
	mu              sync.Mutex
	fast            []*Job
	slow            []*Job
	fastInRow       int
	maxFastPerCycle int
	notifyCh        chan struct{}
}

// newPPQueue constructs an empty ppQueue. maxFastPerCycle must be > 0.
func newPPQueue(maxFastPerCycle int) *ppQueue {
	return &ppQueue{
		maxFastPerCycle: maxFastPerCycle,
		notifyCh:        make(chan struct{}, 1),
	}
}

// PushFast enqueues a job onto the fast (DirectUnpack) queue.
func (q *ppQueue) PushFast(job *Job) {
	q.mu.Lock()
	q.fast = append(q.fast, job)
	q.mu.Unlock()
	q.notify()
}

// PushSlow enqueues a job onto the slow (standard) queue.
func (q *ppQueue) PushSlow(job *Job) {
	q.mu.Lock()
	q.slow = append(q.slow, job)
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

// Len returns (fastLen, slowLen) without locking for long — safe to call
// from the consumer goroutine or from tests.
func (q *ppQueue) Len() (fast, slow int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.fast), len(q.slow)
}

// Empty returns true when both queues are empty.
func (q *ppQueue) Empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.fast) == 0 && len(q.slow) == 0
}

// Cancel removes a job with the given ID from either queue.
// Returns true if the job was found and removed.
func (q *ppQueue) Cancel(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if idx := findJob(q.fast, jobID); idx >= 0 {
		q.fast = append(q.fast[:idx], q.fast[idx+1:]...)
		return true
	}
	if idx := findJob(q.slow, jobID); idx >= 0 {
		q.slow = append(q.slow[:idx], q.slow[idx+1:]...)
		return true
	}
	return false
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
// Returns the next job according to the fast/slow scheduling rule and true,
// or nil and false when ctx is cancelled.
//
// Must be called from exactly one goroutine (the worker).
func (q *ppQueue) Pop(ctx context.Context) (*Job, bool) {
	for {
		// Try to dequeue without waiting.
		if job := q.tryPop(); job != nil {
			return job, true
		}

		// Both queues empty — wait for a push notification or ctx done.
		select {
		case <-ctx.Done():
			return nil, false
		case <-q.notifyCh:
			// Re-arm: there may be more items than the single notification.
			// Loop back and try again.
		}
	}
}

// tryPop applies the scheduling rule and dequeues one job, or returns nil.
func (q *ppQueue) tryPop() *Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	hasFast := len(q.fast) > 0
	hasSlow := len(q.slow) > 0

	if !hasFast && !hasSlow {
		return nil
	}

	// If we have hit the fast-per-cycle limit and there is a slow job
	// waiting, pull from slow and reset the counter.
	if q.fastInRow >= q.maxFastPerCycle && hasSlow {
		job := q.slow[0]
		q.slow = q.slow[1:]
		q.fastInRow = 0
		return job
	}

	// Prefer fast queue.
	if hasFast {
		job := q.fast[0]
		q.fast = q.fast[1:]
		q.fastInRow++
		return job
	}

	// Fall back to slow queue.
	job := q.slow[0]
	q.slow = q.slow[1:]
	q.fastInRow = 0
	return job
}
