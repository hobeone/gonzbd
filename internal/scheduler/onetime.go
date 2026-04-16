package scheduler

import (
	"sync"
	"time"
)

// Oneshot is a fire-once event to be executed at a future wall-clock time.
// The primary use-case is resuming a server after an auto-pause penalty:
// the downloader enqueues a Oneshot, and the scheduler fires it when
// the penalty window expires.
type Oneshot struct {
	FireAt time.Time
	Action string
	Arg    string
}

// OneshotQueue holds pending Oneshot events and is safe for concurrent use.
type OneshotQueue struct {
	mu    sync.Mutex
	items []Oneshot
}

// NewOneshotQueue returns an empty, ready-to-use OneshotQueue.
func NewOneshotQueue() *OneshotQueue {
	return &OneshotQueue{}
}

// Add appends o to the queue. Safe to call from any goroutine.
func (q *OneshotQueue) Add(o Oneshot) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, o)
}

// Due returns and removes all entries whose FireAt is at or before at.
// Safe to call from any goroutine. The returned slice is owned by the
// caller and is never aliased back into the queue.
func (q *OneshotQueue) Due(at time.Time) []Oneshot {
	q.mu.Lock()
	defer q.mu.Unlock()

	var due, remaining []Oneshot
	for _, o := range q.items {
		if !o.FireAt.After(at) {
			due = append(due, o)
		} else {
			remaining = append(remaining, o)
		}
	}
	q.items = remaining
	return due
}

// Len returns the number of pending events. Safe to call from any goroutine.
func (q *OneshotQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}
