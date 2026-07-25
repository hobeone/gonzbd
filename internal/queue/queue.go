package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
)

// ErrNotFound is returned by Queue methods when the given job ID is
// not present.
var ErrNotFound = errors.New("queue: job not found")

// Queue owns the ordered list of active jobs plus the notify channel
// the downloader waits on.
type Queue struct {
	mu         sync.RWMutex
	jobs       []*Job          // ordered: priority-descending at Add time; Reorder may violate
	byID       map[string]*Job // ID -> *Job for O(1) lookup
	removeFile func(string) error

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

	sOpts fsutil.SanitizeOptions
	store Store

	log *slog.Logger
}

// Store returns the underlying SQLite persistence store, or nil if using legacy in-memory/JSON storage.
//
//nolint:ireturn // Store is intentionally an interface exposing persistence storage
func (q *Queue) Store() Store {
	return q.store
}

// IsDirty reports whether the queue has unsaved mutations. It is safe
// for concurrent use and is used by the periodic checkpoint ticker to
// skip unnecessary saves.
func (q *Queue) IsDirty() bool { return q.dirty.Load() }

// Option configures a Queue during construction.
type Option func(*Queue)

// WithStore sets the SQLite persistence store on the Queue.
func WithStore(store Store) Option {
	return func(q *Queue) {
		q.store = store
	}
}

// WithLogger sets a component-scoped logger on the Queue.
func WithLogger(l *slog.Logger) Option {
	return func(q *Queue) {
		if l != nil {
			q.log = l.With("component", "queue")
		}
	}
}

// WithSanitizeOptions sets filesystem sanitization options for job names on the Queue.
func WithSanitizeOptions(opts fsutil.SanitizeOptions) Option {
	return func(q *Queue) {
		q.sOpts = opts
	}
}

// SetSanitizeOptions updates filesystem sanitization options at runtime.
func (q *Queue) SetSanitizeOptions(opts fsutil.SanitizeOptions) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sOpts = opts
}

// New returns an empty, unpaused queue.
func New(opts ...Option) *Queue {
	q := &Queue{
		byID:       make(map[string]*Job),
		notifyCh:   make(chan struct{}, 1),
		log:        slog.Default().With("component", "queue"),
		removeFile: os.Remove,
	}
	for _, o := range opts {
		o(q)
	}
	return q
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

// CountUnfinishedArticles returns the number of articles in the given file
// that are not yet Done. Used by the pipeline to set TotalParts for the
// assembler correctly on resume/retry — only undone articles will be
// dispatched, so TotalParts must match that count.
func (q *Queue) CountUnfinishedArticles(jobID string, fileIdx int) (int, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	job, ok := q.byID[jobID]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	if fileIdx < 0 || fileIdx >= job.manifest.NumFiles() {
		return 0, fmt.Errorf("queue: fileIdx %d out of range for job %s", fileIdx, jobID)
	}
	// Count from the article state directly rather than using
	// file.Pending. Pending tracks !Done && !Emitted, but this
	// method counts !Done (including Emitted articles). The
	// difference matters for resume where Emitted articles
	// shouldn't be counted as "unfinished" yet haven't been
	// durably committed.
	var count int
	lo, hi := job.manifest.FileRange(fileIdx)
	for i := lo; i < hi; i++ {
		if !job.progress.done[i] {
			count++
		}
	}
	return count, nil
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

// HasDownloadableJobs reports whether any job in the queue is still
// actively downloading or waiting to download (i.e. not paused, not in
// post-processing, and not yet complete). Used to decide when NNTP
// connections can be closed.
func (q *Queue) HasDownloadableJobs() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, j := range q.jobs {
		if j.PostProc {
			continue
		}
		if j.Status == constants.StatusPaused {
			continue
		}
		return true
	}
	return false
}

// HasDownloadingJobs reports whether any unpaused job in the queue is currently
// actively downloading articles.
func (q *Queue) HasDownloadingJobs() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.paused {
		return false
	}
	for _, j := range q.jobs {
		if !j.PostProc && j.Status != constants.StatusPaused {
			return true
		}
	}
	return false
}

// HasPostProcJobs reports whether any job in the queue is currently undergoing
// post-processing (par2 repair, verification, extraction).
func (q *Queue) HasPostProcJobs() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, j := range q.jobs {
		if j.PostProc {
			return true
		}
	}
	return false
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

// UniqueName returns the first name in the sequence base, base.1, base.2, …
// for which exists(name) returns false. It is used by queue.Add (under the
// write lock, with an in-queue existence check) and by app.AddJob (outside
// the lock, with a broader filesystem + queue check) to share the same
// renaming logic.
//
// Note: callers that use UniqueName before acquiring the queue lock accept a
// small TOCTOU window — another concurrent Add may claim the chosen name
// between the call and the lock acquisition. queue.Add re-runs the check
// atomically under its own lock, so the queue is always consistent; the
// race is limited to filesystem-path collisions which are benign in practice.
func UniqueName(base string, exists func(string) bool) string {
	name := base
	for i := 1; exists(name); i++ {
		name = fmt.Sprintf("%s.%d", base, i)
	}
	return name
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

	// Ensure Name is unique within the queue (authoritative check under lock).
	job.Name = UniqueName(job.Name, func(name string) bool {
		for _, existing := range q.jobs {
			if existing.Name == name {
				return true
			}
		}
		return false
	})

	// Initialize pending counters from the fresh job's article state
	// (all articles start with Done=false, Emitted=false so
	// Pending == len(Articles) per file).
	job.progress.recompute(job.manifest)

	if q.store != nil {
		if err := q.store.Add(context.Background(), job); err != nil {
			return err
		}
	}

	idx := q.insertByPriorityLocked(job)
	if q.store != nil && idx < len(q.jobs)-1 {
		_ = q.store.ShiftSortKey(context.Background(), job.ID, idx)
	}
	q.byID[job.ID] = job
	q.dirty.Store(true)
	q.notifyLocked()
	return nil
}

// Remove drops the job from the queue and deletes its persistent state file.
func (q *Queue) Remove(id string) error {
	if q.store != nil {
		if err := q.store.Remove(context.Background(), id); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	var jobPath string
	err := func() error {
		q.mu.Lock()
		defer q.mu.Unlock()
		idx, ok := q.indexOfLocked(id)
		if !ok {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		q.removeAtLocked(idx)
		delete(q.byID, id)
		if q.stateDir != "" && q.store == nil {
			jobPath = filepath.Join(q.stateDir, "jobs", id+".json.gz")
		}
		q.dirty.Store(true)
		return nil
	}()
	if err != nil {
		return err
	}

	// --- No lock held below this line ---
	if jobPath != "" {
		_ = q.removeFile(jobPath) // best-effort delete; error is intentionally ignored
	}
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
	if err := q.setStatusLocked(job, constants.StatusPaused); err != nil {
		return err
	}
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
		if err := q.setStatusLocked(job, constants.StatusQueued); err != nil {
			return err
		}
		job.Warning = ""
	}
	q.dirty.Store(true)
	q.notifyLocked()
	return nil
}

// SetPriority changes a job's priority and re-slots it within the queue.
// The job is removed from its current position and re-inserted at the end
// of the new priority tier, matching SABnzbd's "priority" API action.
func (q *Queue) SetPriority(id string, pri constants.Priority) error {
	if !pri.IsValid() {
		return fmt.Errorf("queue: invalid priority %d", pri)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	idx, ok := q.indexOfLocked(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	job := q.jobs[idx]
	job.Priority = pri
	// Remove from current position.
	q.removeAtLocked(idx)
	// Re-insert at the correct position for the new priority.
	newIdx := q.insertByPriorityLocked(job)
	if q.store != nil && newIdx != idx {
		_ = q.store.ShiftSortKey(context.Background(), job.ID, newIdx)
	}
	q.dirty.Store(true)
	q.notifyLocked()
	return nil
}

// SetPP changes a job's post-processing level (0–3).
// Returns ErrNotFound if the job is absent.
func (q *Queue) SetPP(id string, pp int) error {
	if pp < 0 || pp > 3 {
		return fmt.Errorf("queue: invalid post-processing level %d (must be 0-3)", pp)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	job.PP = pp
	q.dirty.Store(true)
	return nil
}

// SetName changes a job's display name, applying cleanup and filesystem
// sanitization rules to ensure the resulting name is safe for filesystem paths.
// Returns ErrNotFound if absent.
func (q *Queue) SetName(id, name string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	name = stripNZBExt(name)
	name = fsutil.CleanupName(name, q.sOpts)
	name = fsutil.SanitizeFolderName(name, q.sOpts)
	job.Name = name
	q.dirty.Store(true)
	return nil
}

// SetScript changes a job's post-processing script. Returns ErrNotFound
// if absent.
func (q *Queue) SetScript(id, script string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	job.Script = script
	q.dirty.Store(true)
	return nil
}

// SetCategory changes a job's category and inherits the new category's PP
// level, script, and priority — matching SABnzbd's change_cat semantics.
// config.FindCategory resolves name against cats (case-insensitive), falling
// back to the "Default" or "*" entry, or BuiltinDefaultCategory if neither
// is present.
//
// The job is re-slotted in the queue if the resolved priority differs from
// the current one, preserving priority-order invariants. Returns ErrNotFound
// if the job is absent.
func (q *Queue) SetCategory(id, cat string, cats []config.CategoryConfig) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	idx, ok := q.indexOfLocked(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	job := q.jobs[idx]
	resolved := config.FindCategory(cats, cat)
	job.Category = resolved.Name
	job.PP = resolved.PP
	job.Script = resolved.Script
	// Re-slot the job when priority changes — mirrors SetPriority.
	newPri := constants.Priority(int8(resolved.Priority)) //nolint:gosec // priority values fit in int8
	if newPri != job.Priority {
		job.Priority = newPri
		q.removeAtLocked(idx)
		newIdx := q.insertByPriorityLocked(job)
		if q.store != nil && newIdx != idx {
			_ = q.store.ShiftSortKey(context.Background(), job.ID, newIdx)
		}
		q.notifyLocked()
	}
	q.dirty.Store(true)
	return nil
}

// SetStatus updates the status of the job with the given ID. Returns
// ErrIllegalStatusTransition if the requested transition is not allowed,
// ErrNotFound if the job is absent.
func (q *Queue) SetStatus(id string, status constants.Status) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := q.setStatusLocked(job, status); err != nil {
		return err
	}
	q.dirty.Store(true)
	return nil
}

// SetStatusIf conditionally updates a job's status. The status is only
// changed if the current status matches ifCurrent AND the edge is legal.
// Returns ErrNotFound if the job is absent; returns nil even if the
// condition didn't match (no error, just a no-op).
func (q *Queue) SetStatusIf(id string, newStatus, ifCurrent constants.Status) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if job.Status != ifCurrent {
		return nil
	}
	if err := q.setStatusLocked(job, newStatus); err != nil {
		return err
	}
	q.dirty.Store(true)
	return nil
}

// setStatusLocked validates and applies a status transition. Must hold
// q.mu for write.
func (q *Queue) setStatusLocked(job *Job, status constants.Status) error {
	if !canTransitionStatus(job.Status, status) {
		return illegalTransition(job.Status, status)
	}
	job.Status = status
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
	if err := q.setStatusLocked(job, constants.StatusVerifying); err != nil {
		return false, err
	}
	job.PostProc = true
	job.manifest.dropMessageIDIndex()
	q.dirty.Store(true)
	return true, nil
}

// MarkJobStarted records the start time of the first download for a job.
// It is a no-op if the job already has a start time.
func (q *Queue) MarkJobStarted(id string, t time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return ErrNotFound
	}
	if job.progress.downloadStarted.IsZero() {
		job.progress.downloadStarted = t
		q.dirty.Store(true)
	}
	return nil
}

// RecordDownload increments the per-server byte count for a job.
func (q *Queue) RecordDownload(id, server string, bytes int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return ErrNotFound
	}
	if job.progress.serverStats == nil {
		job.progress.serverStats = make(map[string]int64)
	}
	job.progress.serverStats[server] += int64(bytes)
	q.dirty.Store(true)
	return nil
}

// UnfinishedArticle is the snapshot record yielded by
// ForEachUnfinishedArticle. It carries the minimum the dispatcher
// needs to target a specific article; full Job state stays behind
// the queue's lock.
type UnfinishedArticle struct {
	JobID       string
	JobStatus   constants.Status
	JobAdded    time.Time
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
		// Skip jobs already handed to post-processing or fully complete.
		// Without this, aborted/hopeless jobs continue wasting bandwidth.
		if job.PostProc || job.progress.pendingArticles == 0 {
			continue
		}
		m := job.manifest
		for fi := range m.NumFiles() {
			// Deferred files (on-demand par2 recovery volumes) are held back.
			// They already have Pending == 0 (set by recompute), so the
			// next check skips them too; the explicit guard documents intent
			// and protects against future counter drift.
			if job.progress.files[fi].Complete || job.progress.files[fi].Pending == 0 || job.progress.files[fi].Deferred {
				continue
			}
			lo, hi := m.FileRange(fi)
			for i := lo; i < hi; i++ {
				if job.progress.done[i] || job.progress.emitted[i] {
					continue
				}
				if !fn(UnfinishedArticle{
					JobID:       job.ID,
					JobStatus:   job.Status,
					JobAdded:    job.Added,
					FileIdx:     fi,
					MessageID:   m.ArticleID(i),
					Bytes:       m.ArticleBytes(i),
					Subject:     m.FileSubject(fi),
					FailedBytes: job.progress.failedBytes,
					Par2Bytes:   m.Par2Bytes(),
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
// Prefer MarkArticlesDone for batch operations — it takes the write lock
// once for the entire batch rather than once per article.
func (q *Queue) MarkArticleDone(jobID, messageID string) error {
	return q.MarkArticlesDone(jobID, []string{messageID})
}

// MarkArticleEmitted flags an article as having a result in flight from the
// downloader to the assembler. This is a transient, in-memory-only bit
// (JobProgress's emitted array, never persisted): its purpose is to
// prevent the dispatcher from re-dispatching the same article between the
// moment the downloader sends
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
	i, ok := job.manifest.articleIndexByID(messageID)
	if !ok {
		return fmt.Errorf("%w: article %s in job %s", ErrNotFound, messageID, jobID)
	}
	job.progress.markEmitted(job.manifest, i)
	return nil
}

// ClearArticleEmitted resets the transient Emitted flag on a single article,
// allowing the dispatcher to re-dispatch it. This is used when the pipeline
// receives a retryable download error: the article was emitted but did not
// succeed on this attempt, so it must be returned to the dispatch pool.
// Wakes the dispatcher so the article is picked up on the next pass.
func (q *Queue) ClearArticleEmitted(jobID, messageID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	i, ok := job.manifest.articleIndexByID(messageID)
	if !ok {
		return fmt.Errorf("%w: article %s in job %s", ErrNotFound, messageID, jobID)
	}
	// Only restore to pending if the article is not already done. An article
	// can have Emitted=true and Done=true when MarkArticlesDone ran before
	// ClearArticleEmitted (e.g. a late assembler flush after a downloader
	// reload). In that case the article is finished and must not be counted
	// as pending.
	job.progress.clearEmitted(job.manifest, i)
	q.notifyLocked()
	return nil
}

// ClearAllEmitted resets transient article state for a downloader reload.
// This must be called when the downloader is reloaded: articles that were
// Emitted by the old downloader but never completed (because the old
// downloader was stopped) would otherwise be permanently skipped by
// ForEachUnfinishedArticle.
//
// Additionally, articles marked Failed during the old downloader's
// teardown (e.g. from context cancellation) are reset to retryable
// state. Without this, a late assembler flush could race ahead and
// permanently mark articles as Done+Failed, preventing re-dispatch.
// Only articles with Failed=true are reset; successfully completed
// articles (Done=true, Failed=false) are preserved.
func (q *Queue) ClearAllEmitted() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, job := range q.jobs {
		// Reset Downloading → Queued: the old downloader is gone, so no
		// articles are actually in-flight. The new downloader's first
		// dispatch pass will transition back to Downloading.
		if job.Status == constants.StatusDownloading {
			_ = q.setStatusLocked(job, constants.StatusQueued)
		}
		m := job.manifest
		for i := range m.NumArticles() {
			// Reset articles that were marked Failed during the old
			// downloader's teardown so they can be retried by the
			// new downloader. Successfully completed articles
			// (Done && !Failed) are left untouched.
			job.progress.resetForReload(m, i)
		}
		// Recompute pending counters from ground truth after bulk
		// state reset. Incremental tracking is fragile here because
		// both Emitted and Failed flags are being cleared in bulk.
		job.progress.recompute(m)
	}
	// Drain any stale notification (e.g. from MarkArticleDone calls during
	// pipeline drain of the old downloader's buffered results) so our fresh
	// notification is guaranteed to be delivered.
	select {
	case <-q.notifyCh:
	default:
	}
	q.notifyLocked()
}

// TotalRemainingBytes returns the sum of RemainingBytes across all jobs.
func (q *Queue) TotalRemainingBytes() int64 {
	q.mu.RLock()
	defer q.mu.RUnlock()
	var total int64
	for _, job := range q.byID {
		total += job.progress.remainingBytes
	}
	return total
}

// CheckEarlyAbort checks whether the job should be aborted based on the
// first-article failure rate heuristic. Returns true exactly once per
// job when the threshold is exceeded.
func (q *Queue) CheckEarlyAbort(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return false
	}
	return job.IsEarlyAbort()
}

// MarkArticleFailed marks an article as Done and increments the FailedBytes
// count. Returns (true, nil) if it was the first time this article was marked
// done. Delegates to MarkArticlesFailed so both forms share identical counter
// logic and cannot drift.
func (q *Queue) MarkArticleFailed(jobID, messageID string) (bool, error) {
	firstTime, err := q.MarkArticlesFailed(jobID, []string{messageID})
	return len(firstTime) > 0, err
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
	job, ok := q.byID[jobID]
	if !ok {
		q.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	var notFound []string
	for _, id := range messageIDs {
		i, ok := job.manifest.articleIndexByID(id)
		if !ok {
			notFound = append(notFound, id)
			continue
		}
		// If the article was Emitted, Pending was already decremented;
		// if it was still pending (!Emitted), decrement now. Per-file
		// progress only counts successful completions: markDone is not
		// called for articles MarkArticlesFailed has already flagged
		// Failed, so this method's articles are by definition successful.
		job.progress.markDone(job.manifest, i)
	}
	q.dirty.Store(true)
	q.mu.Unlock()
	// --- No lock held below this line ---
	for _, id := range notFound {
		q.log.Warn("MarkArticlesDone: article not found", "job", jobID, "msgid", id)
	}
	return nil
}

// SetFileWriteCursor records the assembler's contiguous write frontier for a
// file as a persisted resume hint (see JobFile.WriteCursor). Called from the
// assembler's batched flush, never per-article.
func (q *Queue) SetFileWriteCursor(jobID string, fileIdx int, cursor int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	if fileIdx < 0 || fileIdx >= job.manifest.NumFiles() {
		return fmt.Errorf("queue: fileIdx %d out of range for job %s", fileIdx, jobID)
	}
	job.progress.files[fileIdx].WriteCursor = cursor
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
	job, ok := q.byID[jobID]
	if !ok {
		q.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	firstTime := make([]string, 0, len(messageIDs))
	var notFound []string
	for _, id := range messageIDs {
		i, ok := job.manifest.articleIndexByID(id)
		if !ok {
			notFound = append(notFound, id)
			continue
		}
		if job.progress.markFailed(job.manifest, i) {
			firstTime = append(firstTime, id)
		}
	}
	var failedBytes, par2Bytes int64
	var releasedPar2 bool
	if len(firstTime) > 0 {
		failedBytes, par2Bytes = job.progress.failedBytes, job.manifest.Par2Bytes()
		q.dirty.Store(true)
		// On-demand par2: a permanent data-article failure proves this job
		// will need repair. Release the deferred recovery volumes now — while
		// the connection is live and the articles are freshest — rather than
		// waiting for the download-complete verify. Par2Recovered guards it to
		// fire once; Par2Files>0 skips the scan for jobs without par2.
		if !job.progress.par2Recovered && job.manifest.Par2Files() > 0 {
			if q.undeferRecoveryLocked(job, job.progress.DeferredRecoveryIndices()) {
				job.progress.par2ReleaseReason = "permanent article download failure detected on active queue"
				releasedPar2 = true
			}
		}
		q.notifyLocked()
	}
	q.mu.Unlock()
	// --- No lock held below this line ---
	for _, id := range notFound {
		q.log.Warn("MarkArticlesFailed: article not found", "job", jobID, "msgid", id)
	}
	if len(firstTime) > 0 {
		q.log.Warn("articles marked FAILED", "job", jobID, "count", len(firstTime), "failed_bytes", failedBytes, "par2_bytes", par2Bytes)
		if releasedPar2 {
			q.log.Info("on-demand par2: download failure detected, releasing recovery volumes early", "job", jobID)
		}
	}
	return firstTime, nil
}

// UndeferRecoveryVolumes clears the Deferred flag on the given file indices of
// jobID, re-activating their articles for dispatch, and recomputes the pending
// download-complete gate does not re-fire after the volumes arrive. Indices
// that are not deferred are ignored; out-of-range indices return an error.
// Returns ErrNotFound if the job is absent.
//
// The fileIdxs argument is an explicit set so callers control the policy:
// Phase 1 passes the full deferred set (Job.DeferredRecoveryIndices); Phase 2
// passes a block-covering subset. The mutation, counter recomputation, and
// completion semantics are identical for both.
func (q *Queue) UndeferRecoveryVolumes(jobID string, fileIdxs []int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	for _, fi := range fileIdxs {
		if fi < 0 || fi >= job.manifest.NumFiles() {
			return fmt.Errorf("queue: fileIdx %d out of range for job %s", fi, jobID)
		}
	}
	q.undeferRecoveryLocked(job, fileIdxs)
	return nil
}

// SetPar2ReleaseReason sets the Par2ReleaseReason field for the given job.
func (q *Queue) SetPar2ReleaseReason(jobID, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	job.progress.par2ReleaseReason = reason
	q.dirty.Store(true)
	return nil
}

// DiscardDeferredPar2 removes all deferred par2 files from the job. Since
// Manifest is immutable and shared by reference across snapshots, this
// rebuilds a fresh Manifest (with recomputed TotalBytes, but Par2Bytes/
// Par2Files carried over unchanged) and a re-indexed JobProgress cloned
// from the old one, rather than editing either in place. A pure no-op
// (no rebuild, no dirty flag) when there is nothing deferred to discard.
func (q *Queue) DiscardDeferredPar2(jobID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}

	m := job.manifest
	var activeFiles []JobFile
	var discardedBytes int64
	for fi := range m.NumFiles() {
		if job.progress.files[fi].Deferred {
			discardedBytes += m.FileBytes(fi)
			continue
		}
		lo, hi := m.FileRange(fi)
		articles := make([]JobArticle, 0, hi-lo)
		for i := lo; i < hi; i++ {
			articles = append(articles, JobArticle{
				ID:     m.ArticleID(i),
				Bytes:  m.ArticleBytes(i),
				Number: m.ArticleNumber(i),
			})
		}
		activeFiles = append(activeFiles, JobFile{
			Subject:        m.FileSubject(fi),
			Date:           m.FileDate(fi),
			Bytes:          m.FileBytes(fi),
			Articles:       articles,
			IsPar2Recovery: m.FileIsPar2Recovery(fi),
		})
	}

	if discardedBytes > 0 {
		// Manifest is shared by reference across every snapshot, so it
		// cannot be filtered in place — build a fresh one instead. Every
		// surviving article's global index shifts whenever a removed file
		// preceded it, so JobProgress's flat arrays must be re-indexed to
		// match, not merely recomputed.
		newManifestVal := newManifest(activeFiles)
		// Par2Bytes/Par2Files are carried over from the old manifest
		// unchanged, not recomputed against the reduced file set — this
		// deliberately leaves them stale, still counting the just-removed
		// recovery volumes.
		newManifestVal.par2Bytes = m.Par2Bytes()
		newManifestVal.par2Files = m.Par2Files()

		// Clone-and-adjust the old JobProgress rather than constructing
		// fresh: a fresh newJobProgress(newManifestVal) would zero every
		// job-level scalar (ServerStats, DownloadStarted/Finished, ...),
		// silently discarding progress on a partially-downloaded job.
		newProgress := job.progress.clone()
		newDone := make([]bool, 0, newManifestVal.NumArticles())
		newFailed := make([]bool, 0, newManifestVal.NumArticles())
		newEmitted := make([]bool, 0, newManifestVal.NumArticles())
		newFiles := make([]FileProgress, 0, newManifestVal.NumFiles())
		for fi := range m.NumFiles() {
			if job.progress.files[fi].Deferred {
				continue
			}
			// A Deferred file's articles are never dispatched
			// (ForEachUnfinishedArticle skips them), so they are always
			// Done=false/Failed=false/Emitted=false at discard time —
			// dropping them loses nothing.
			lo, hi := m.FileRange(fi)
			newDone = append(newDone, job.progress.done[lo:hi]...)
			newFailed = append(newFailed, job.progress.failed[lo:hi]...)
			newEmitted = append(newEmitted, job.progress.emitted[lo:hi]...)
			newFiles = append(newFiles, job.progress.files[fi])
		}
		newProgress.done = newDone
		newProgress.failed = newFailed
		newProgress.emitted = newEmitted
		newProgress.files = newFiles
		newProgress.remainingBytes -= discardedBytes
		if newProgress.remainingBytes < 0 {
			newProgress.remainingBytes = 0
		}
		// pendingArticles/articlesResolved/articlesFailed and each file's
		// Pending/BytesDownloaded are already correct here (deferred files
		// contribute nothing, so dropping them changes nothing these
		// counters depend on), but recompute defensively rather than
		// relying on that invariant staying true — every other bulk-state
		// path (Add, Load, ClearAllEmitted, undeferRecoveryLocked) does the
		// same.
		newProgress.recompute(newManifestVal)

		job.manifest = newManifestVal
		job.progress = newProgress
		q.dirty.Store(true)
	}
	return nil
}

// undeferRecoveryLocked clears Deferred on the given file indices of job. If
// any file changed it marks Par2Recovered, recomputes pending counters from
// ground truth (RemainingBytes already counted these bytes), and wakes the
// dispatcher. Indices that are out of range or not deferred are ignored.
// Must be called with q.mu held for writing. Returns true if anything changed.
func (q *Queue) undeferRecoveryLocked(job *Job, fileIdxs []int) bool {
	changed := false
	for _, fi := range fileIdxs {
		if fi < 0 || fi >= job.manifest.NumFiles() || !job.progress.files[fi].Deferred {
			continue
		}
		job.progress.files[fi].Deferred = false
		changed = true
	}
	if changed {
		job.progress.par2Recovered = true
		job.progress.recompute(job.manifest)
		q.dirty.Store(true)
		q.notifyLocked()
	}
	return changed
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
	if fileIdx < 0 || fileIdx >= job.manifest.NumFiles() {
		return fmt.Errorf("queue: fileIdx %d out of range for job %s", fileIdx, jobID)
	}
	job.progress.files[fileIdx].Complete = true
	q.dirty.Store(true)
	return nil
}

// SetFileFilename stores the resolved final filename on a JobFile.
func (q *Queue) SetFileFilename(jobID string, fileIdx int, filename string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	if fileIdx < 0 || fileIdx >= job.manifest.NumFiles() {
		return fmt.Errorf("queue: fileIdx %d out of range for job %s", fileIdx, jobID)
	}
	job.progress.files[fileIdx].Filename = filename
	q.dirty.Store(true)
	return nil
}

// SetFileCRC32 stores the assembled CRC32 on a JobFile. The CRC is
// computed by the assembler by combining per-article yEnc CRCs in
// offset order and represents the CRC32 of the entire file as written
// to disk. This is used during QuickCheck to verify file integrity
// against par2 file hashes without re-reading from disk.
func (q *Queue) SetFileCRC32(jobID string, fileIdx int, crc uint32) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	if fileIdx < 0 || fileIdx >= job.manifest.NumFiles() {
		return fmt.Errorf("queue: fileIdx %d out of range for job %s", fileIdx, jobID)
	}
	job.progress.files[fileIdx].AssembledCRC32 = crc
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

// Reorder moves a job to newIndex in the queue, shifting other jobs
// accordingly. It emits a "queue_updated" event and wakes the downloader
// so priority/order changes take effect immediately.
//
// newIndex is clamped to [0, len-1]. Manual reordering may leave the
// queue no longer strictly priority-sorted; subsequent Add calls
// still place new jobs by priority, which may interleave with the
// user's manual ordering. The downloader treats slice order as
// authoritative either way.
func (q *Queue) Reorder(id string, newIndex int) error {
	if q.store != nil {
		if err := q.store.ShiftSortKey(context.Background(), id, newIndex); err != nil {
			return err
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	idx, ok := q.indexOfLocked(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	job := q.jobs[idx]
	q.removeAtLocked(idx)
	// Clamp after removal so the valid range is [0, len(q.jobs)].
	// slices.Insert at len(q.jobs) appends to the end.
	newIndex = max(0, min(newIndex, len(q.jobs)))
	q.jobs = slices.Insert(q.jobs, newIndex, job)
	q.dirty.Store(true)
	q.notifyLocked()
	return nil
}

// removeAtLocked removes q.jobs[idx] in O(N), nil-zeroing the vacated slot
// so the GC can collect the *Job pointer. Assumes q.mu is held for write.
func (q *Queue) removeAtLocked(idx int) {
	copy(q.jobs[idx:], q.jobs[idx+1:])
	last := len(q.jobs) - 1
	q.jobs[last] = nil // allow GC of removed *Job
	q.jobs = q.jobs[:last]
}

// insertByPriorityLocked inserts job at the end of its priority tier.
// Higher priority values sort earlier. Assumes q.mu is held for write.
func (q *Queue) insertByPriorityLocked(job *Job) int {
	// Find the first position where the existing job has strictly
	// lower priority than the new one; insert before it. This places
	// the new job at the end of its priority tier when the queue is
	// already sorted.
	i, _ := slices.BinarySearchFunc(q.jobs, job, func(existing, target *Job) int {
		// Descending order: higher priority comes first.
		// Return -1 while existing.Priority >= target.Priority (keep going),
		// +1 when existing.Priority < target.Priority (insert here).
		if existing.Priority >= target.Priority {
			return -1
		}
		return 1
	})
	q.jobs = slices.Insert(q.jobs, i, job)
	return i
}

func (q *Queue) indexOfLocked(id string) (int, bool) {
	for i, j := range q.jobs {
		if j.ID == id {
			return i, true
		}
	}
	return 0, false
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
