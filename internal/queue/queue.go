package queue

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/hobeone/sabnzbd-go/internal/constants"
)

// ErrNotFound is returned by Queue methods when the given job ID is
// not present.
var ErrNotFound = errors.New("queue: job not found")

// Queue owns the ordered list of active jobs plus the notify channel
// the downloader waits on.
type Queue struct {
	mu   sync.RWMutex
	jobs []*Job          // ordered: priority-descending at Add time; Reorder may violate
	byID map[string]*Job // ID -> *Job for O(1) lookup

	// paused is a queue-wide pause flag. Independent of per-job
	// Status == StatusPaused: when paused=true the downloader should
	// not dispatch any articles regardless of per-job state.
	paused bool

	// notifyCh is a cap-1 channel the Queue sends to whenever new
	// downloadable work appears. Sends are non-blocking so callers
	// can safely call notifyLocked while holding mu; a slow consumer
	// can coalesce multiple signals into one wake-up.
	notifyCh chan struct{}
}

// New returns an empty, unpaused queue.
func New() *Queue {
	return &Queue{
		byID:     make(map[string]*Job),
		notifyCh: make(chan struct{}, 1),
	}
}

// Notify returns the downloader wake-up channel. Cap 1; signals
// coalesce. Callers should not close the returned channel.
func (q *Queue) Notify() <-chan struct{} { return q.notifyCh }

// Len returns the number of jobs currently in the queue.
func (q *Queue) Len() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.jobs)
}

// IsPaused reports the queue-wide pause flag.
func (q *Queue) IsPaused() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.paused
}

// Get returns the job with the given ID or ErrNotFound.
func (q *Queue) Get(id string) (*Job, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	job, ok := q.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return job, nil
}

// List returns a snapshot slice of the queue's jobs in current order.
// The returned slice is a fresh allocation; callers can iterate it
// without holding the queue lock. The *Job pointers inside alias the
// queue's storage and must not be mutated directly.
func (q *Queue) List() []*Job {
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := make([]*Job, len(q.jobs))
	copy(out, q.jobs)
	return out
}

// Add inserts job into the queue. The job is placed at the end of its
// priority tier (see insertByPriority). Returns an error if the job's
// ID collides with one already in the queue.
func (q *Queue) Add(job *Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.byID[job.ID]; exists {
		return fmt.Errorf("queue: job %q already present", job.ID)
	}
	q.insertByPriorityLocked(job)
	q.byID[job.ID] = job
	q.notifyLocked()
	return nil
}

// Remove drops the job from the queue. No-op on the notify channel
// because removal reduces work; downloaders that were already waiting
// don't need to be woken to discover less to do.
func (q *Queue) Remove(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	idx, ok := q.indexOfLocked(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	q.jobs = append(q.jobs[:idx], q.jobs[idx+1:]...)
	delete(q.byID, id)
	return nil
}

// Pause marks a specific job as paused. The downloader checks
// Status != StatusPaused before dispatching articles.
func (q *Queue) Pause(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	job.Status = constants.StatusPaused
	return nil
}

// Resume flips a paused job back to Queued and signals the downloader.
func (q *Queue) Resume(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if job.Status == constants.StatusPaused {
		job.Status = constants.StatusQueued
	}
	q.notifyLocked()
	return nil
}

// PauseAll sets the queue-wide pause flag. Existing downloads
// currently in flight are not cancelled; the downloader simply stops
// dispatching new articles.
func (q *Queue) PauseAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paused = true
}

// ResumeAll clears the queue-wide pause flag and signals the
// downloader.
func (q *Queue) ResumeAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paused = false
	q.notifyLocked()
}

// Reorder moves the job with the given ID to newIndex in the queue.
// newIndex is clamped to [0, len-1]. Manual reordering may leave the
// queue no longer strictly priority-sorted; subsequent Add calls
// still place new jobs by priority, which may interleave with the
// user's manual ordering. The downloader treats slice order as
// authoritative either way.
func (q *Queue) Reorder(id string, newIndex int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	idx, ok := q.indexOfLocked(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if newIndex < 0 {
		newIndex = 0
	}
	if newIndex >= len(q.jobs) {
		newIndex = len(q.jobs) - 1
	}
	if newIndex == idx {
		return nil
	}
	job := q.jobs[idx]
	q.jobs = append(q.jobs[:idx], q.jobs[idx+1:]...)
	q.jobs = append(q.jobs[:newIndex], append([]*Job{job}, q.jobs[newIndex:]...)...)
	q.notifyLocked()
	return nil
}

// insertByPriorityLocked inserts job at the end of its priority tier.
// Higher priority values sort earlier. Assumes q.mu is held for write.
func (q *Queue) insertByPriorityLocked(job *Job) {
	// Find the first position where the existing job has strictly
	// lower priority than the new one; insert before it. This places
	// the new job at the end of its priority tier when the queue is
	// already sorted.
	i := sort.Search(len(q.jobs), func(i int) bool {
		return q.jobs[i].Priority < job.Priority
	})
	q.jobs = append(q.jobs, nil)
	copy(q.jobs[i+1:], q.jobs[i:])
	q.jobs[i] = job
}

func (q *Queue) indexOfLocked(id string) (int, bool) {
	for i, j := range q.jobs {
		if j.ID == id {
			return i, true
		}
	}
	return -1, false
}

// notifyLocked fires a non-blocking signal on notifyCh. Must be
// called with q.mu held (read or write); the non-blocking send never
// blocks even if the channel is full.
func (q *Queue) notifyLocked() {
	select {
	case q.notifyCh <- struct{}{}:
	default:
	}
}
