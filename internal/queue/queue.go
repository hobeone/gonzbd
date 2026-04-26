package queue

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

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

	stateDir string // Root directory for persistent state (admin/queue)

	// paused is a queue-wide pause flag. Independent of per-job
	// Status == StatusPaused: when paused=true the downloader should
	// not dispatch any articles regardless of per-job state.
	paused bool

	// notifyCh is a cap-1 channel the Queue sends to whenever new
	// downloadable work appears. Sends are non-blocking so callers
	// can safely call notifyLocked while holding mu; a slow consumer
	// can coalesce multiple signals into one wake-up.
	notifyCh chan struct{}

	// dirty is set to true by the five article/file mutation methods
	// (MarkArticleDone, MarkArticleFailed, MarkFileComplete,
	// MarkArticlesDone, MarkArticlesFailed) and cleared by Save on a
	// successful write. The periodic checkpoint ticker no-ops when
	// dirty is false, avoiding unnecessary I/O on idle queues.
	dirty atomic.Bool
}

// IsDirty reports whether the queue has unsaved mutations. It is safe
// for concurrent use and is used by the periodic checkpoint ticker to
// skip unnecessary saves.
func (q *Queue) IsDirty() bool { return q.dirty.Load() }

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

// GetJobStatus returns the lifecycle state of the job with the given
// ID. Returns ErrNotFound if the job is absent. Safe for concurrent use.
func (q *Queue) GetJobStatus(id string) (constants.Status, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	job, ok := q.byID[id]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return job.Status, nil
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

// ExistsByName reports whether a job with the given name is present in
// the queue. Case-sensitive.
func (q *Queue) ExistsByName(name string) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, j := range q.jobs {
		if j.Name == name {
			return true
		}
	}
	return false
}

// ExistsByMD5 reports whether a job with the given MD5 is present in
// the queue.
func (q *Queue) ExistsByMD5(md5 string) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, j := range q.jobs {
		if j.MD5 == md5 {
			return true
		}
	}
	return false
}

// Add inserts job into the queue. The job is placed at the end of its
// priority tier (see insertByPriority). Returns an error if the job's
// ID collides with one already in the queue.
//
// If a job with the same Name already exists, the new job is renamed
// by appending .1, .2, etc. to match Python SABnzbd behavior.
func (q *Queue) Add(job *Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.byID[job.ID]; exists {
		return fmt.Errorf("queue: job %q already present", job.ID)
	}

	// Ensure Name is unique in the queue.
	baseName := job.Name
	for i := 1; ; i++ {
		found := false
		for _, existing := range q.jobs {
			if existing.Name == job.Name {
				found = true
				break
			}
		}
		if !found {
			break
		}
		job.Name = fmt.Sprintf("%s.%d", baseName, i)
	}

	q.insertByPriorityLocked(job)
	q.byID[job.ID] = job
	q.dirty.Store(true)
	q.notifyLocked()
	return nil
}

// Remove drops the job from the queue and deletes its persistent state file.
func (q *Queue) Remove(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	idx, ok := q.indexOfLocked(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	q.jobs = append(q.jobs[:idx], q.jobs[idx+1:]...)
	delete(q.byID, id)

	if q.stateDir != "" {
		jobPath := filepath.Join(q.stateDir, "jobs", id+".json.gz")
		_ = os.Remove(jobPath)
	}
	q.dirty.Store(true)
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
	q.dirty.Store(true)
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
		job.Warning = ""
	}
	q.dirty.Store(true)
	q.notifyLocked()
	return nil
}

// SetStatus updates the status of the job with the given ID. Returns
// ErrNotFound if the job is absent.
func (q *Queue) SetStatus(id string, status constants.Status) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	job.Status = status
	q.dirty.Store(true)
	return nil
}

// SetPostProcStarted marks the job as being in post-processing.
// Returns true if the flag was successfully set (first time), false
// if it was already set.
func (q *Queue) SetPostProcStarted(id string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if job.PostProc {
		return false, nil
	}
	job.PostProc = true
	job.Status = constants.StatusQueued
	q.dirty.Store(true)
	return true, nil
}

// MarkJobStarted records the start time of the first download for a job.
// It is a no-op if the job already has a start time.
func (q *Queue) MarkJobStarted(id string, t time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return
	}
	if job.DownloadStarted.IsZero() {
		job.DownloadStarted = t
		q.dirty.Store(true)
	}
}

// RecordDownload increments the per-server byte count for a job.
func (q *Queue) RecordDownload(id string, server string, bytes int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return
	}
	if job.ServerStats == nil {
		job.ServerStats = make(map[string]int64)
	}
	job.ServerStats[server] += int64(bytes)
	q.dirty.Store(true)
}

// UnfinishedArticle is the snapshot record yielded by
// ForEachUnfinishedArticle. It carries the minimum the dispatcher
// needs to target a specific article; full Job state stays behind
// the queue's lock.
type UnfinishedArticle struct {
	JobID       string
	JobStatus   constants.Status
	FileIdx     int
	MessageID   string
	Bytes       int
	Subject     string
	FailedBytes int64
	Par2Bytes   int64
}

// ForEachUnfinishedArticle invokes fn for every not-yet-Done article
// in the queue, in priority/file/article order. The read lock is
// held across the whole iteration — fn must not call back into the
// Queue (e.g. Add, Remove, MarkArticleDone) or it will deadlock.
//
// fn returns false to stop iteration early; this mirrors Go's
// iter.Seq convention and is useful when the caller is bounded (e.g.
// the dispatcher gives up once all work channels are full).
//
// Paused jobs are yielded too; the caller decides whether to skip
// them. Passing the filter decision to the caller keeps this method
// oblivious to higher-level policy.
func (q *Queue) ForEachUnfinishedArticle(fn func(UnfinishedArticle) bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, job := range q.jobs {
		for fi := range job.Files {
			file := &job.Files[fi]
			if file.Complete {
				continue
			}
			for ai := range file.Articles {
				art := &job.Files[fi].Articles[ai]
				if art.Done || art.Emitted {
					continue
				}
				if !fn(UnfinishedArticle{
					JobID:       job.ID,
					JobStatus:   job.Status,
					FileIdx:     fi,
					MessageID:   art.ID,
					Bytes:       art.Bytes,
					Subject:     file.Subject,
					FailedBytes: job.FailedBytes,
					Par2Bytes:   job.Par2Bytes,
				}) {
					return
				}
			}
		}
	}
}

// MarkArticleDone flips the Done flag on the article with the given
// Message-ID within jobID. Returns ErrNotFound if either the job or
// the article is absent.
//
// The dispatcher calls this from its worker goroutines as articles
// complete successfully. Taking the write lock here funnels article
// state mutation through a single well-known path, keeping callers
// from holding direct pointers to Job internals.
//
// Flag semantics: setting Done on an already-done article is a no-op
// (idempotent); the method does not track downgrade-from-done because
// no code path currently needs to undo a completion.
func (q *Queue) MarkArticleDone(jobID, messageID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	for fi := range job.Files {
		for ai := range job.Files[fi].Articles {
			if job.Files[fi].Articles[ai].ID == messageID {
				if !job.Files[fi].Articles[ai].Done {
					job.Files[fi].Articles[ai].Done = true
					job.RemainingBytes -= int64(job.Files[fi].Articles[ai].Bytes)
					slog.Debug("article done (success)", "msgid", messageID, "job", jobID, "remaining", job.RemainingBytes)
				}
				q.dirty.Store(true)
				return nil
			}
		}
	}
	return fmt.Errorf("%w: article %s in job %s", ErrNotFound, messageID, jobID)
}

// MarkArticleEmitted flags an article as having a result in flight from the
// downloader to the assembler. This is a transient, in-memory-only bit
// (see JobArticle.Emitted): its purpose is to prevent the dispatcher from
// re-dispatching the same article between the moment the downloader sends
// a result on the completions channel and the moment the assembler makes
// the outcome durable (MarkArticleDone / MarkArticleFailed). On restart
// the flag is lost, so any article whose bytes weren't fsynced is
// re-downloaded — that's the B.6 durability invariant.
//
// Idempotent: setting Emitted on an article that is already Emitted, Done,
// or Failed is a no-op. Returns ErrNotFound if the job/article is absent.
func (q *Queue) MarkArticleEmitted(jobID, messageID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	for fi := range job.Files {
		for ai := range job.Files[fi].Articles {
			art := &job.Files[fi].Articles[ai]
			if art.ID == messageID {
				art.Emitted = true
				return nil
			}
		}
	}
	return fmt.Errorf("%w: article %s in job %s", ErrNotFound, messageID, jobID)
}

// TotalRemainingBytes returns the sum of RemainingBytes across all jobs.
func (q *Queue) TotalRemainingBytes() int64 {
	q.mu.RLock()
	defer q.mu.RUnlock()
	var total int64
	for _, job := range q.byID {
		total += job.RemainingBytes
	}
	return total
}

// MarkArticleFailed marks an article as Done and increments the FailedBytes
// count. It also decrements the remaining byte count of the job so that
// hopeless jobs can be identified by comparing FailedBytes vs Par2Bytes.
// Returns (true, nil) if it was the first time this article was marked done.
func (q *Queue) MarkArticleFailed(jobID, messageID string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	for fi := range job.Files {
		for ai := range job.Files[fi].Articles {
			art := &job.Files[fi].Articles[ai]
			if art.ID == messageID {
				if !art.Done {
					art.Done = true
					art.Failed = true
					job.FailedBytes += int64(art.Bytes)
					job.RemainingBytes -= int64(art.Bytes)
					slog.Warn("article marked FAILED", "msgid", messageID, "job", jobID, "failed_bytes", job.FailedBytes, "par2_bytes", job.Par2Bytes)
					q.dirty.Store(true)
					return true, nil
				}
				q.dirty.Store(true)
				return false, nil
			}
		}
	}
	return false, fmt.Errorf("%w: article %s in job %s", ErrNotFound, messageID, jobID)
}

// MarkArticlesDone is the batched form of MarkArticleDone. It flips
// Done on every article in messageIDs for jobID under a single write
// lock. Articles already Done are skipped (no double-decrement of
// RemainingBytes). Missing message-IDs are logged but do not abort the
// batch; the method only errors if the job itself is not found.
//
// The single-lock-per-batch is the whole point: under a heavy
// completions firehose the assembler previously took the queue write
// lock once per article, serialising the hot path against every
// RLock-reader (UI snapshots, dispatcher iteration). B.7 amortises
// that to one lock acquisition per flush.
func (q *Queue) MarkArticlesDone(jobID string, messageIDs []string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	// Build a quick lookup so we scan the job's articles once rather
	// than len(messageIDs) times.
	want := make(map[string]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		want[id] = struct{}{}
	}
	for fi := range job.Files {
		for ai := range job.Files[fi].Articles {
			art := &job.Files[fi].Articles[ai]
			if _, ok := want[art.ID]; !ok {
				continue
			}
			delete(want, art.ID)
			if art.Done {
				continue
			}
			art.Done = true
			job.RemainingBytes -= int64(art.Bytes)
		}
	}
	for id := range want {
		slog.Warn("MarkArticlesDone: article not found", "job", jobID, "msgid", id)
	}
	q.dirty.Store(true)
	return nil
}

// MarkArticlesFailed is the batched form of MarkArticleFailed. Like
// MarkArticlesDone it takes the queue write lock exactly once. The
// returned firstTimeIDs lists message-IDs that actually flipped from
// not-Done to Done-Failed this call — callers that need the
// "first-time failure" semantics of the singular form (e.g. for event
// emission) should consult that list; duplicate or unknown IDs are
// silently dropped from it.
func (q *Queue) MarkArticlesFailed(jobID string, messageIDs []string) ([]string, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	want := make(map[string]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		want[id] = struct{}{}
	}
	firstTime := make([]string, 0, len(messageIDs))
	for fi := range job.Files {
		for ai := range job.Files[fi].Articles {
			art := &job.Files[fi].Articles[ai]
			if _, ok := want[art.ID]; !ok {
				continue
			}
			delete(want, art.ID)
			if art.Done {
				continue
			}
			art.Done = true
			art.Failed = true
			job.FailedBytes += int64(art.Bytes)
			job.RemainingBytes -= int64(art.Bytes)
			firstTime = append(firstTime, art.ID)
		}
	}
	for id := range want {
		slog.Warn("MarkArticlesFailed: article not found", "job", jobID, "msgid", id)
	}
	if len(firstTime) > 0 {
		slog.Warn("articles marked FAILED", "job", jobID, "count", len(firstTime), "failed_bytes", job.FailedBytes, "par2_bytes", job.Par2Bytes)
	}
	q.dirty.Store(true)
	return firstTime, nil
}

// MarkFileComplete marks the file at fileIdx within jobID as fully assembled
// and complete. Returns ErrNotFound if the job or index is invalid.
func (q *Queue) MarkFileComplete(jobID string, fileIdx int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	if fileIdx < 0 || fileIdx >= len(job.Files) {
		return fmt.Errorf("queue: fileIdx %d out of range for job %s", fileIdx, jobID)
	}
	job.Files[fileIdx].Complete = true
	q.dirty.Store(true)
	return nil
}

// PauseAll sets the queue-wide pause flag. Existing downloads
// currently in flight are not cancelled; the downloader simply stops
// dispatching new articles.
func (q *Queue) PauseAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paused = true
	q.dirty.Store(true)
}

// ResumeAll clears the queue-wide pause flag and signals the
// downloader.
func (q *Queue) ResumeAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paused = false
	q.dirty.Store(true)
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
	q.dirty.Store(true)
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
