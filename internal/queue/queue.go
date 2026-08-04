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
	"github.com/hobeone/gonzbd/internal/history"
)

// ErrNotFound is returned by Queue methods when the given job ID is
// not present.
var ErrNotFound = errors.New("queue: job not found")

// ErrJobNotResident is returned by Queue methods when the job exists but its
// Manifest and JobProgress are not currently in memory. This is an ordinary
// condition, not a failure: Add de-hydrates every StatusQueued job when a
// store or state directory is configured, and PromoteNext re-hydrates a job
// when it is promoted to active. Callers that only act on resident jobs should
// treat it the way they treat ErrNotFound.
//
// Kept distinct from ErrNotFound so a caller can tell "no such job" from "job
// is not currently resident". The downloader suppresses both when logging, but
// collapsing them into one sentinel would also mask genuine lookup failures.
var ErrJobNotResident = errors.New("queue: job not resident")

// ErrManifestStale is returned when the manifest stored on disk contradicts
// the job's own JobProgress — different file or article counts — so the two
// cannot be paired.
//
// Reporting it rather than tolerating it is not optional: handing a
// mismatched pair to JobProgress.recompute panics by design, and hydration
// runs on background goroutines that carry no recover. Hydration used to
// sidestep the question by rebuilding progress from whatever it read; now
// that it preserves the live progress, the disagreement is visible.
//
// It had one ordinary cause until #294. DiscardDeferredPar2 rebuilt a smaller
// manifest and progress in memory when on-demand par2 verification came back
// clean, and never rewrote the .json.gz, so every discard left the file on
// disk describing the pre-discard job. It now persists the rebuilt manifest
// and job_files rows together.
//
// What remains is a torn write. Store.ReplaceManifest writes a blob and
// commits a transaction, and no ordering makes those two atomic across the
// filesystem and SQLite — a crash between them leaves the same mismatch, now
// bounded by one fsync plus one small transaction rather than arising on
// every discard.
//
// Distinct from ErrJobNotResident because it is not ordinary: the job's
// persisted state is internally inconsistent and re-reading will not fix it.
var ErrManifestStale = errors.New("queue: stored manifest does not match the job's progress")

// Queue owns the ordered list of active jobs plus the notify channel
// the downloader waits on.
type Queue struct {
	mu        sync.RWMutex
	jobs      []*Job          // ordered: priority-descending at Add time; Reorder may violate
	byID      map[string]*Job // ID -> *Job for O(1) lookup
	promoting map[string]bool // job ID -> true for jobs currently in promotion loop

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

	activeSet *ActiveSet
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
		if sq, ok := store.(*SQLiteStore); ok && q.stateDir == "" {
			q.stateDir = sq.Dir()
		}
	}
}

// WithStateDir sets the persistent state directory on the Queue.
func WithStateDir(dir string) Option {
	return func(q *Queue) {
		q.stateDir = dir
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

// WithMaxActiveJobs sets the maximum number of active jobs on the Queue.
func WithMaxActiveJobs(maxActive int) Option {
	return func(q *Queue) {
		if maxActive > 0 {
			q.activeSet.SetMaxActive(maxActive)
		}
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
		byID:      make(map[string]*Job),
		promoting: make(map[string]bool),
		notifyCh:  make(chan struct{}, 1),
		log:       slog.Default().With("component", "queue"),
		activeSet: NewActiveSet(4),
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

// residentJob returns the job for id only if its Manifest is currently in
// memory. Callers must already hold q.mu — either lock, since this only
// reads.
//
// New and modified methods that dereference job.manifest should obtain their
// job through this rather than indexing q.byID directly. A job present in
// q.byID is not necessarily hydrated: Add releases the manifest for
// StatusQueued jobs, and evictJobLocked releases it when a job leaves the
// active set, in both cases leaving the job in the map.
//
// This is now a property of the whole file, not just the direction of
// travel: every manifest-tier mutation routes through here, so none of them
// can report success for work it did not do (#261). That is enforced rather
// than asserted — TestManifestAccessIsGated walks the package AST and fails
// any *Queue method that dereferences job.manifest without calling this,
// because hand searches over this surface have three times now returned a
// different subset of it.
//
// The progress-tier methods deliberately do not route through here.
// JobProgress is permanently resident, so gating them on residency would
// refuse work they are always able to perform. Adding a residency check to a
// method that reads only progress is a bug in the same family, not caution:
// SetPar2ReleaseReason had one, and it silently dropped the reason a job's
// par2 volumes were released for every non-resident job.
//
// Manifest is the only residency signal now: JobProgress is permanently
// resident (docs/queue-lifecycle.md) and never nil for a job in q.byID, so a
// non-resident job is manifest-nil/progress-live, not both-nil — that state
// is the ordinary steady state for every StatusQueued/StatusPaused job, not
// a half-hydration hazard. Skipping work on ErrJobNotResident is still safe,
// for a reason that no longer depends on progress's presence: every caller
// through this gate needs the manifest itself to resolve what it was asked
// to mutate (a message ID or article index only means something against a
// resident Manifest), so there is no correct mutation to perform without one.
// And should a caller somehow bypass that, whatever it wrote would not
// survive anyway — the moment the job returns to resident, hydrateJobLocked
// unconditionally rebuilds JobProgress from the manifest plus
// Store.RestoreJobProgress's persisted counters, discarding whatever was
// live beforehand. The skipped mutation is moot because promotion
// supersedes it, not because progress was about to vanish.
//
// The job.progress == nil clause below is not load-bearing for any known
// caller — no code path leaves a job in q.byID with nil progress — but it
// stays as defense in depth: everything downstream of this gate assumes
// job.progress is safe to dereference, and every worker goroutine that could
// hit a violation carries no recover(), so the cost of keeping a redundant
// check is a single comparison against the cost of a process crash if that
// invariant is ever broken elsewhere.
func (q *Queue) residentJob(id string) (*Job, error) {
	job, ok := q.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if job.manifest == nil || job.progress == nil {
		return nil, fmt.Errorf("%w: %s", ErrJobNotResident, id)
	}
	return job, nil
}

// CountUnfinishedArticles returns the number of articles in the given file
// that are not yet Done. Used by the pipeline to set TotalParts for the
// assembler correctly on resume/retry — only undone articles will be
// dispatched, so TotalParts must match that count.
//
// Returns ErrNotFound if the job is absent, or ErrJobNotResident if it exists
// but is not currently hydrated. Callers must not read the returned count when
// err is non-nil: a zero would be indistinguishable from "this file is fully
// downloaded".
func (q *Queue) CountUnfinishedArticles(jobID string, fileIdx int) (int, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	// Report non-residency as an error rather than a zero count: callers read
	// a successful 0 as "this file has nothing left to download", which for a
	// merely de-hydrated job is wrong and silent.
	job, err := q.residentJob(jobID)
	if err != nil {
		return 0, err
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
		if !job.progress.done.Get(i) {
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
	// Enforce the residency invariant at the single entry point that puts a
	// job into q.byID, rather than re-checking job.progress == nil at every
	// read site downstream (e.g. TotalRemainingBytes, called from
	// runMetricsPush — a 1Hz ticker goroutine with no recover()). Every
	// production caller (NewJob followed by Add) already sets
	// progress; a nil here means a caller bypassed both, e.g. a bare
	// &Job{Status: StatusQueued} representing a job not yet hydrated from
	// disk — a shape some claim-failure tests use deliberately to exercise
	// PromoteNext's own hydration-failure path. Repair rather than reject:
	// size progress from the manifest if one is already attached, or to
	// zero articles if not (the same "nothing known yet" shape
	// hydrateJobLocked expects to overwrite once it actually reads the
	// manifest). Either way, no job with nil progress reaches q.byID.
	if job.progress == nil {
		m := job.manifest
		if m == nil {
			m = &Manifest{}
		}
		job.progress = newJobProgress(m)
	}
	if q.store == nil && q.stateDir != "" && job.manifest != nil {
		manifestsDir := filepath.Join(q.stateDir, "manifests")
		_ = os.MkdirAll(manifestsDir, 0o750)
		manifestPath := filepath.Join(manifestsDir, job.ID+".json.gz")
		_ = writeGzJSON(manifestPath, job.manifest)
	}

	q.mu.Lock()
	if _, exists := q.byID[job.ID]; exists {
		q.mu.Unlock()
		return fmt.Errorf("queue: job %q already present", job.ID)
	}

	job.Name = UniqueName(job.Name, func(name string) bool {
		for _, existing := range q.jobs {
			if existing.Name == name {
				return true
			}
		}
		return false
	})

	if job.manifest != nil && job.progress != nil {
		job.progress.recompute(job.manifest)
	}

	if m := job.manifest; m != nil {
		job.setScalarsFromManifest(m)
	}

	// Holding q.mu across the store writes below is intentional: it prevents
	// a TOCTOU name collision between the uniqueness check above and the
	// insert, and keeps the RAM and SQLite views consistent before the
	// downloader can dispatch against a half-inserted job.
	if q.store != nil {
		if err := q.store.Add(context.Background(), job); err != nil { //lockio: TOCTOU-free insert; see comment above
			q.mu.Unlock()
			return err
		}
	}

	idx := q.insertByPriorityLocked(job)
	if q.store != nil && idx < len(q.jobs)-1 {
		_ = q.store.ShiftSortKey(context.Background(), job.ID, idx) //lockio: same insert critical section as store.Add above
	}
	q.byID[job.ID] = job

	if job.Status == constants.StatusQueued && (q.store != nil || q.stateDir != "") {
		// Release only the manifest. Progress stays resident — see
		// docs/queue-lifecycle.md: JobProgress is cheap (bitsets) relative to
		// the manifest and every reporting path depends on it never being
		// absent.
		job.setResidency(nil, job.progress)
	}

	q.dirty.Store(true)
	q.notifyLocked()
	q.mu.Unlock()

	q.PromoteNext(context.Background())
	return nil
}

// Remove drops the job from the queue and deletes its persistent state file.
func (q *Queue) Remove(id string) error {
	if q.store != nil {
		if err := q.store.Remove(context.Background(), id); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	err := func() error {
		q.mu.Lock()
		defer q.mu.Unlock()
		idx, ok := q.indexOfLocked(id)
		if !ok {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		job := q.jobs[idx]
		q.evictJobLocked(job)
		q.removeAtLocked(idx)
		delete(q.byID, id)
		q.dirty.Store(true)
		return nil
	}()
	if err != nil {
		return err
	}

	// --- No lock held below this line ---
	// Unlink the manifest/progress artifacts only now that id has left
	// q.byID (evictJobLocked + delete above already ran under q.mu). Doing
	// this earlier — or under q.mu, which check_lock_io flags as I/O — would
	// reopen the exact race this ordering exists to close: Queue.Snapshot
	// clones a job under RLock and hydrates it from disk after releasing the
	// lock, so an unlink that races ahead of the job's removal from q.byID
	// can catch a snapshot mid-hydration.
	if q.store != nil {
		if err := q.store.DeleteJobArtifacts(context.Background(), id); err != nil {
			q.log.Warn("failed to delete job artifacts after remove", "job_id", id, "err", err)
		}
	}
	q.PromoteNext(context.Background())
	return nil
}

// Pause marks a specific job as paused. The downloader checks
// Status != StatusPaused before dispatching articles.
func (q *Queue) Pause(id string) error {
	q.mu.Lock()
	job, ok := q.byID[id]
	if !ok {
		q.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := q.setStatusLocked(job, constants.StatusPaused); err != nil {
		q.mu.Unlock()
		return err
	}
	q.evictJobLocked(job)
	q.dirty.Store(true)
	q.mu.Unlock()

	q.PromoteNext(context.Background())
	return nil
}

// Resume flips a paused job back to Queued and signals the downloader.
func (q *Queue) Resume(id string) error {
	q.mu.Lock()
	job, ok := q.byID[id]
	if !ok {
		q.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if job.Status == constants.StatusPaused {
		if err := q.setStatusLocked(job, constants.StatusQueued); err != nil {
			q.mu.Unlock()
			return err
		}
		job.Warning = ""
	}
	q.dirty.Store(true)
	q.notifyLocked()
	q.mu.Unlock()

	q.PromoteNext(context.Background())
	return nil
}

// Retry resets a failed job back to Queued state and triggers promotion.
//
// A failed job is ordinarily non-resident (StatusFailed is not a resident
// status; SetStatus's transition into it evicted manifest/progress — see
// evictJobLocked). Job.ResetForRetry needs live Progress to distinguish
// "failed, retry me" from "done, keep me": it only resets an article whose
// failed[] bit is set, and that flag only exists once the job is hydrated.
// So Retry hydrates the job itself first — via hydrateJobLocked, not
// hydrateResidentLocked, since StatusQueued is not a resident status and
// hydrateResidentLocked's activeSet.Add would wrongly mark a merely-queued
// job as active before PromoteNext has actually dispatched it.
//
// hydrateJobLocked runs before setStatusLocked so a hydration failure (a
// missing/corrupt manifest) leaves job.Status untouched and returns an
// error, matching the fail-closed contract SetStatus/SetStatusIf already
// uphold for issue #264 — Retry must not silently no-op or partially mutate
// the job.
//
// Once reset, the corrected progress is persisted to the store before
// PromoteNext runs (still under q.mu, matching the two lockio precedents in
// PromoteNext below). PromoteNext unconditionally calls
// Store.RestoreJobProgress for every job it promotes, including one that is
// already resident, so skipping this write would let PromoteNext re-read the
// stale on-disk articles_done and silently undo the reset one layer down
// (the exact failure mode issue #260 describes).
func (q *Queue) Retry(id string) error {
	q.mu.Lock()
	job, ok := q.byID[id]
	if !ok {
		q.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if job.Status == constants.StatusFailed {
		if err := q.hydrateJobLocked(job, id); err != nil { //lockio: reads manifest + calls store.RestoreJobProgress; see hydrateJobLocked doc, tracked in #229
			q.mu.Unlock()
			return fmt.Errorf("queue: retry job %s: %w", id, err)
		}
		prevStatus, prevWarning := job.Status, job.Warning
		// Snapshot progress before ResetForRetry mutates it in place. If the
		// persist below fails, the store row is untouched by that mutation,
		// so this snapshot — not the mutated live object — is what the
		// rolled-back job must show; see the rollback comment below.
		prevProgress := job.progress.clone()
		if err := q.setStatusLocked(job, constants.StatusQueued); err != nil {
			q.mu.Unlock()
			return err
		}
		job.Warning = ""
		job.ResetForRetry()
		if q.store != nil {
			if err := q.store.Update(context.Background(), job); err != nil { //lockio: persists the reset articles_done before PromoteNext's unconditional RestoreJobProgress can re-read the stale row
				// The write is load-bearing, so its failure cannot be
				// discarded: PromoteNext would re-read the stale row and undo
				// the reset, leaving Retry to report success on a job that is
				// still failed and undispatchable — the exact defect #260
				// describes. Roll the job back instead.
				//
				// Progress must never be nil (docs/queue-lifecycle.md), so the
				// rollback restores prevProgress rather than dropping
				// residency to nil as it used to. The un-written store row
				// still reflects prevProgress, not ResetForRetry's optimistic
				// reset, so keeping the mutated live object resident would
				// let TotalRemainingBytes and friends report numbers nothing
				// on disk backs up.
				job.Status, job.Warning = prevStatus, prevWarning
				job.setResidency(job.manifest, prevProgress)
				q.evictJobLocked(job)
				q.mu.Unlock()
				return fmt.Errorf("queue: retry job %s: persist reset: %w", id, err)
			}
		}
	}
	q.dirty.Store(true)
	q.notifyLocked()
	q.mu.Unlock()

	q.PromoteNext(context.Background())
	return nil
}

// ActiveSet returns the queue's ActiveSet manager.
func (q *Queue) ActiveSet() *ActiveSet {
	return q.activeSet
}

// SetMaxActiveJobs updates the active job limit and triggers promotion.
func (q *Queue) SetMaxActiveJobs(maxActive int) {
	q.activeSet.SetMaxActive(maxActive)
	q.PromoteNext(context.Background())
}

// PromoteNext promotes eligible Queued jobs to Downloading until ActiveSet is full.
// Safe for concurrent calls. Disk I/O during manifest loading is unlocked.
func (q *Queue) PromoteNext(ctx context.Context) {
	for {
		// Step 1: Under lock, check capacity and reserve next candidate queued job.
		q.mu.Lock()
		if q.activeSet.Len()+len(q.promoting) >= q.activeSet.MaxActive() {
			q.mu.Unlock()
			break
		}

		candidate := q.findNextQueuedCandidateLocked()
		if candidate == nil {
			q.mu.Unlock()
			break // No more queued jobs ready for promotion
		}

		jobID := candidate.ID
		q.promoting[jobID] = true
		stateDir := q.stateDir

		var cachedManifest *Manifest
		if candidate.manifest != nil {
			m := *candidate.manifest
			cachedManifest = &m
		}
		q.mu.Unlock()

		// Step 2: Unlocked Disk I/O to read manifest.
		manifestPath := filepath.Join(stateDir, "manifests", jobID+".json.gz")
		var manifest Manifest
		var readErr error
		if cachedManifest != nil {
			manifest = *cachedManifest
		} else {
			readErr = readGzJSON(manifestPath, &manifest)
		}

		// Step 3: Re-acquire lock to handle result.
		q.mu.Lock()
		delete(q.promoting, jobID)

		// Re-verify job still exists and is still in StatusQueued
		job, ok := q.byID[jobID]
		if !ok || job.Status != constants.StatusQueued {
			q.mu.Unlock()
			continue
		}

		if readErr != nil {
			cf := q.prepareClaimFailureLocked(job, manifestPath, readErr)
			q.mu.Unlock()
			q.finishClaimFailure(ctx, cf)
			q.log.Error("failed to claim manifest during promotion, transitioning to FAILED",
				"job_id", jobID,
				"manifest_path", manifestPath,
				"err", readErr,
			)
			continue // Continue promotion loop for next candidate
		}

		// Attach manifest & progress if not already resident
		if job.manifest == nil {
			manifest.buildMessageIDIndex()
			job.setResidency(&manifest, newJobProgress(&manifest))
			// A job promoted here after a restart (StatusQueued, restored via
			// SQLiteStore.Get/Load without a manifest) never had these
			// scalars populated. Backfill from the manifest just read above —
			// no extra I/O, it already happened in Step 2.
			job.setScalarsFromManifest(&manifest)
		}

		// If SQLite store present, restore per-file progress counters.
		//
		// This SELECT runs under q.mu because RestoreJobProgress mutates
		// job.progress in place; hoisting it needs a detached load plus an
		// attach under lock. Deliberate and tracked as a follow-up. The
		// trailing marker is inert today -- check_lock_io has no detector for
		// Store or database/sql calls -- but is in place for when it gains one.
		if q.store != nil {
			if err := q.store.RestoreJobProgress(ctx, job); err != nil { //lockio: mutates job.progress in place; hoist tracked as follow-up
				// ErrManifestStale means the stored rows and the stored
				// manifest describe different file sets, not that the
				// manifest is unreadable. Setting it aside as ".corrupt"
				// would be a lie about a file that parses, and would destroy
				// the newer of the two artifacts — the one reconciliation
				// works from. SQLiteStore.Get reconciles a torn pair at load,
				// so reaching here means the tear appeared after load; fail
				// the job, but truthfully and without the rename.
				setAside := manifestPath
				claimErr := fmt.Errorf("restore progress: %w", err)
				if errors.Is(err, ErrManifestStale) {
					setAside = ""
				}
				cf := q.prepareClaimFailureLocked(job, setAside, claimErr)
				q.mu.Unlock()
				q.finishClaimFailure(ctx, cf)
				q.log.Error("failed to restore job progress during promotion, transitioning to FAILED",
					"job_id", jobID,
					"err", err,
				)
				continue
			}
		}

		if err := q.setStatusLocked(job, constants.StatusDownloading); err != nil {
			q.mu.Unlock()
			q.log.Error("failed to set StatusDownloading during promotion", "job_id", jobID, "err", err)
			continue
		}

		q.activeSet.Add(job)
		// Runs under q.mu so the RAM and SQLite views of the Downloading
		// transition cannot diverge. Tracked as a follow-up alongside
		// RestoreJobProgress above; see the note there about the marker.
		if q.store != nil {
			_ = q.store.Update(ctx, job) //lockio: keeps RAM and SQLite views of the transition consistent
		}
		q.dirty.Store(true)
		q.notifyLocked()
		q.mu.Unlock()
	}
}

func (q *Queue) findNextQueuedCandidateLocked() *Job {
	if q.paused {
		return nil
	}
	for _, j := range q.jobs {
		if j.Status == constants.StatusQueued && !j.PostProc && !q.activeSet.IsResident(j.ID) && !q.promoting[j.ID] {
			return j
		}
	}
	return nil
}

func (q *Queue) evictJobLocked(job *Job) {
	if job == nil {
		return
	}
	q.activeSet.Evict(job)
	if q.store != nil || q.stateDir != "" {
		// Release only the manifest; progress stays resident (see Add's
		// matching comment and docs/queue-lifecycle.md).
		job.setResidency(nil, job.progress)
	}
}

// claimFailure carries the I/O a failed manifest claim still owes once the
// queue lock has been released. Every field is captured under q.mu, so
// finishClaimFailure reads no shared queue state.
type claimFailure struct {
	job          *Job
	entry        history.Entry
	manifestPath string
	// store is nil when the queue has no SQLite store configured.
	store Store
}

// prepareClaimFailureLocked marks a job failed and evicts it from the in-RAM
// queue. It deliberately performs no disk or database I/O: the caller must
// release q.mu and then hand the returned value to finishClaimFailure.
//
// Splitting it this way keeps a SQLite transaction (Store.MoveToHistory spans
// the active-job and history tables) off the global queue mutex, which the
// downloader's dispatch loop contends on every pass.
func (q *Queue) prepareClaimFailureLocked(job *Job, manifestPath string, claimErr error) claimFailure {
	// "Corrupt" is a claim about the file, and it is wrong for a manifest
	// that parses but disagrees with the stored rows. Both messages reach the
	// operator — Warning in the queue, FailMessage in history — so the two
	// causes have to read differently or the wrong one gets investigated.
	reason := "Corrupt manifest"
	failMessage := "Unreadable or corrupt manifest"
	if errors.Is(claimErr, ErrManifestStale) {
		reason = "Stored manifest and file rows disagree"
		failMessage = "Stored manifest and file rows describe different file sets"
	}
	job.Warning = fmt.Sprintf("%s: %v", reason, claimErr)
	job.Status = constants.StatusFailed

	cf := claimFailure{
		job:          job,
		manifestPath: manifestPath,
		store:        q.store,
	}
	if q.store != nil {
		cf.entry = history.Entry{
			NzoID:       job.ID,
			Name:        job.Name,
			Category:    job.Category,
			Status:      string(constants.StatusFailed),
			FailMessage: fmt.Sprintf("%s: %v", failMessage, claimErr),
			Completed:   time.Now().UTC(),
		}
	}

	idx, ok := q.indexOfLocked(job.ID)
	if ok {
		q.removeAtLocked(idx)
	}
	delete(q.byID, job.ID)
	q.dirty.Store(true)
	return cf
}

// finishClaimFailure performs the disk and database I/O for a failed claim.
// It must be called with q.mu released.
//
// Recovery is best-effort: the job has already been evicted from the in-RAM
// queue, so there is nothing to roll back and no caller in a position to act
// on a returned error. The failures are logged rather than discarded, because
// each one leaves observable divergence — a stale .corrupt rename, or a job
// still sitting in SQLite as Queued while absent from RAM — that is otherwise
// invisible until the next startup reconciles it.
func (q *Queue) finishClaimFailure(ctx context.Context, cf claimFailure) {
	if _, err := os.Stat(cf.manifestPath); err == nil {
		if err := os.Rename(cf.manifestPath, cf.manifestPath+".corrupt"); err != nil {
			q.log.Warn("could not set aside corrupt manifest",
				"job_id", cf.job.ID,
				"manifest_path", cf.manifestPath,
				"err", err,
			)
		}
	}
	if cf.store != nil {
		if err := cf.store.MoveToHistory(ctx, cf.job, cf.entry); err != nil {
			q.log.Error("could not move failed job to history; it is evicted from "+
				"the queue but may remain in the store until startup reconciles it",
				"job_id", cf.job.ID,
				"err", err,
			)
		}
	}
}

// SetPriority changes a job's priority and re-slots it within the queue.
// The job is removed from its current position and re-inserted at the end
// of the new priority tier, matching SABnzbd's "priority" API action.
func (q *Queue) SetPriority(id string, pri constants.Priority) error {
	if !pri.IsValid() {
		return fmt.Errorf("queue: invalid priority %d", pri)
	}
	var newIdx int
	var shiftNeeded bool
	err := func() error {
		q.mu.Lock()
		defer q.mu.Unlock()
		idx, ok := q.indexOfLocked(id)
		if !ok {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		job := q.jobs[idx]
		job.Priority = pri
		q.removeAtLocked(idx)
		newIdx = q.insertByPriorityLocked(job)
		shiftNeeded = (q.store != nil && newIdx != idx)
		q.dirty.Store(true)
		q.notifyLocked()
		return nil
	}()
	if err != nil {
		return err
	}
	if shiftNeeded {
		_ = q.store.ShiftSortKey(context.Background(), id, newIdx)
	}
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
	var newIdx int
	var shiftNeeded bool
	err := func() error {
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
		newPri := constants.Priority(int8(resolved.Priority)) //nolint:gosec // priority values fit in int8
		if newPri != job.Priority {
			job.Priority = newPri
			q.removeAtLocked(idx)
			newIdx = q.insertByPriorityLocked(job)
			shiftNeeded = (q.store != nil && newIdx != idx)
			q.notifyLocked()
		}
		q.dirty.Store(true)
		return nil
	}()
	if err != nil {
		return err
	}
	if shiftNeeded {
		_ = q.store.ShiftSortKey(context.Background(), id, newIdx)
	}
	return nil
}

// hydrateResidentLocked re-hydrates job's manifest and progress when a
// transition moves it into a resident status while it is still de-hydrated
// (job.manifest == nil). Must be called with q.mu held for write, after
// job.Status has already been set to status by setStatusLocked; prevStatus
// is the status job held immediately before that call, used to revert on
// failure. No-ops (returns nil, no mutation) when status is not a resident
// status, when the job is already hydrated, or when the queue has no
// persistence configured (in that configuration jobs are never de-hydrated
// in the first place, so manifest is never nil here).
//
// On failure it reverts job.Status to prevStatus rather than routing through
// prepareClaimFailureLocked/finishClaimFailure (issue #264's suggested
// direction): that machinery additionally evicts the job from the queue and
// moves it to history, a bigger and more destructive transition than what a
// caller requesting e.g. StatusDownloading asked for, and it would leave
// job.Status at Failed rather than unchanged — breaking the "no partial
// mutation on error" contract SetStatus/SetStatusIf must uphold. Reverting
// and surfacing the error instead means the requested transition simply
// doesn't happen, while still upholding residentJob's invariant that
// manifest and progress are always either both nil or both non-nil.
func (q *Queue) hydrateResidentLocked(job *Job, id string, status, prevStatus constants.Status) error {
	if !isResidentStatus(status) || job.manifest != nil {
		return nil
	}
	if err := q.hydrateJobLocked(job, id); err != nil {
		job.Status = prevStatus
		return fmt.Errorf("queue: hydrate job %s for status %s: %w", id, status, err)
	}
	q.activeSet.Add(job)
	return nil
}

// hydrateJobLocked loads job's manifest and (if a store is configured)
// per-file progress counters from persistence into memory, when the job is
// currently non-resident (job.manifest == nil). Must be called with q.mu
// held for write.
//
// Unlike hydrateResidentLocked, this does not touch job.Status and does not
// add the job to the ActiveSet — it is the shared, status-agnostic
// hydration primitive. hydrateResidentLocked layers status-transition
// semantics (activeSet.Add, reverting job.Status on failure) on top of it
// for callers moving a job into a resident status; Retry uses this directly
// because it needs a hydrated job while transitioning into StatusQueued,
// which is not a resident status and must not be added to the ActiveSet
// before PromoteNext actually dispatches it.
//
// No-ops (returns nil) when the job is already hydrated or when the queue
// has no persistence configured (in that configuration jobs are never
// de-hydrated in the first place).
func (q *Queue) hydrateJobLocked(job *Job, id string) error {
	if job.manifest != nil {
		return nil
	}
	if q.store == nil && q.stateDir == "" {
		return nil
	}
	manifestPath := filepath.Join(q.stateDir, "manifests", id+".json.gz")
	// Preserve the progress that was live before this hydration attempt
	// (per docs/queue-lifecycle.md it is never nil) so a failed read or
	// RestoreJobProgress below has something accurate to fall back to
	// instead of nil.
	priorProgress := job.progress
	var m Manifest
	if err := readGzJSON(manifestPath, &m); err != nil {
		// Record why on the job itself, not only in the returned error.
		// The caller learns of this failure, but every later Manifest()
		// call on this same live q.jobs entry would otherwise report the
		// generic ErrJobNotResident — indistinguishable from the routine
		// eviction that every queued and paused job is already in.
		hydrateErr := fmt.Errorf("queue: hydrate job %s: %w", id, err)
		job.setHydrateFailure(priorProgress, hydrateErr)
		return hydrateErr
	}
	// Keep the progress the job already carries rather than rebuilding it,
	// for the same reason as hydrateSnapshot: since #276 a non-resident job's
	// JobProgress is live and accurate, and rebuilding discarded whatever
	// only memory knew — on-demand par2's deferral among it (#287). With a
	// store this was masked, because RestoreJobProgress below re-read the
	// deferred column; without one it was the original bug on the live job.
	//
	// The same staleness hazard applies, so the same guard does. A manifest
	// read from disk can predate a DiscardDeferredPar2 that shrank the
	// in-memory pair, and pairing them panics inside recompute. Fail closed
	// rather than crash: this path already fails closed for an unreadable
	// manifest, and a manifest that contradicts the job is no more usable
	// than one that will not parse.
	if !priorProgress.describesSameJobAs(&m) {
		hydrateErr := fmt.Errorf(
			"%w: stored manifest for job %s describes %d files/%d articles but its progress holds %d/%d",
			ErrManifestStale, id, m.NumFiles(), m.NumArticles(),
			len(priorProgress.files), priorProgress.done.Len())
		job.setHydrateFailure(priorProgress, hydrateErr)
		return hydrateErr
	}
	job.setResidency(&m, priorProgress)
	// A job restored via SQLiteStore.Get/Load while non-resident
	// (StatusQueued/StatusPaused) never had these scalars populated — they
	// are only set on the resident-status branch of those paths. Backfill
	// them here from the manifest this call already loaded, rather than
	// leaving them zero for the rest of the job's in-memory lifetime; this
	// adds no extra I/O since the manifest read above already happened.
	job.setScalarsFromManifest(&m)
	if q.store != nil {
		if err := q.store.RestoreJobProgress(context.Background(), job); err != nil { //lockio: restores progress in place; hoist tracked in #229
			// newJobProgress built an all-zero JobProgress that RestoreJobProgress
			// was meant to fill in from the stored counters. Keeping it on a
			// failed restore would present a part-downloaded job as having
			// downloaded nothing, and Retry persists what it hydrates, so the
			// empty progress would then be written back over the real one.
			// Drop the manifest and report the failure — PromoteNext already
			// fails closed on this same call — but restore priorProgress
			// rather than nil: progress must never be absent, and
			// priorProgress is the one value here that is actually backed by
			// what's on disk. Record the reason on the job for the same
			// reason as the read branch above.
			hydrateErr := fmt.Errorf("queue: restore progress for job %s: %w", id, err)
			job.setHydrateFailure(priorProgress, hydrateErr)
			return hydrateErr
		}
	}
	return nil
}

// SetStatus updates the status of the job with the given ID. Returns
// ErrIllegalStatusTransition if the requested transition is not allowed,
// ErrNotFound if the job is absent, or a wrapped error if the transition
// requires re-hydrating the job (see hydrateResidentLocked) and that fails —
// in which case job.Status is left exactly as it was before the call.
func (q *Queue) SetStatus(id string, status constants.Status) error {
	q.mu.Lock()
	job, ok := q.byID[id]
	if !ok {
		q.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	prevStatus := job.Status
	if err := q.setStatusLocked(job, status); err != nil {
		q.mu.Unlock()
		return err
	}
	if err := q.hydrateResidentLocked(job, id, status, prevStatus); err != nil { //lockio: reads manifest + calls store.RestoreJobProgress; see hydrateResidentLocked doc, tracked in #229
		q.mu.Unlock()
		return err
	}
	if !isResidentStatus(status) {
		q.evictJobLocked(job)
	}
	q.dirty.Store(true)
	q.mu.Unlock()

	q.PromoteNext(context.Background())
	return nil
}

// SetStatusIf conditionally updates a job's status. The status is only
// changed if the current status matches ifCurrent AND the edge is legal.
// Returns ErrNotFound if the job is absent; returns nil even if the
// condition didn't match (no error, just a no-op). Like SetStatus, returns a
// wrapped error and leaves job.Status untouched if re-hydration is required
// and fails (see hydrateResidentLocked).
func (q *Queue) SetStatusIf(id string, newStatus, ifCurrent constants.Status) error {
	q.mu.Lock()
	job, ok := q.byID[id]
	if !ok {
		q.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if job.Status != ifCurrent {
		q.mu.Unlock()
		return nil
	}
	prevStatus := job.Status
	if err := q.setStatusLocked(job, newStatus); err != nil {
		q.mu.Unlock()
		return err
	}
	if err := q.hydrateResidentLocked(job, id, newStatus, prevStatus); err != nil { //lockio: reads manifest + calls store.RestoreJobProgress; see hydrateResidentLocked doc, tracked in #229
		q.mu.Unlock()
		return err
	}
	if !isResidentStatus(newStatus) {
		q.evictJobLocked(job)
	}
	q.dirty.Store(true)
	q.mu.Unlock()

	q.PromoteNext(context.Background())
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
	if job.progress.downloadFinished.IsZero() {
		job.progress.downloadFinished = time.Now().UTC()
	}
	if q.store != nil {
		_ = q.store.Update(context.Background(), job) //lockio: keeps RAM and SQLite views of the PostProc transition consistent; tracked in #229
	}
	if job.manifest != nil {
		job.manifest.dropMessageIDIndex()
	}
	q.dirty.Store(true)
	return true, nil
}

// MarkDownloadFinished records the download completion time for a job.
func (q *Queue) MarkDownloadFinished(id string, t time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	// Progress tier: no residency guard. The IsZero test is a business
	// condition (first finish wins), not a check for absence — progress is
	// permanently resident.
	if job.progress.downloadFinished.IsZero() {
		job.progress.downloadFinished = t
		if q.store != nil {
			_ = q.store.Update(context.Background(), job) //lockio: keeps RAM and SQLite views of the finish timestamp consistent; tracked in #229
		}
		q.dirty.Store(true)
	}
	return nil
}

// MarkJobStarted records the start time of the first download for a job.
// It is a no-op if the job already has a start time.
func (q *Queue) MarkJobStarted(id string, t time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	// Progress tier: see MarkDownloadFinished. IsZero is "first start wins",
	// not a residency check.
	if job.progress.downloadStarted.IsZero() {
		job.progress.downloadStarted = t
		if q.store != nil {
			_ = q.store.Update(context.Background(), job) //lockio: keeps RAM and SQLite views of the start timestamp consistent; tracked in #229
		}
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
	// No residency guard: per-server byte counts live on JobProgress, which
	// is permanently resident, and Add repairs any job entering q.byID with
	// nil progress. Downloaded bytes must be recorded whether or not the
	// manifest happens to be in memory.
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
	ArtIdx      int32 // Global article index within Manifest
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
		// Skip jobs already handed to post-processing or fully complete, or
		// non-resident jobs (no manifest to walk articles against). This
		// used to gate on job.progress == nil, which was equivalent to
		// manifest non-residency back when eviction dropped both together;
		// now that progress is always resident (docs/queue-lifecycle.md),
		// gating on progress here would let a non-resident job (nil
		// manifest, non-nil progress) through to the m.NumFiles() call
		// below and panic.
		if job.PostProc || job.manifest == nil || job.progress.pendingArticles == 0 {
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
				if job.progress.done.Get(i) || job.progress.emitted.Get(i) {
					continue
				}
				if !fn(UnfinishedArticle{
					JobID:       job.ID,
					JobStatus:   job.Status,
					JobAdded:    job.Added,
					FileIdx:     fi,
					ArtIdx:      int32(i), //nolint:gosec // article index bounded by manifest length
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
// or Failed is a no-op. Returns ErrNotFound if the job/article is absent, or
// ErrJobNotResident if the job exists but is not currently hydrated.
func (q *Queue) MarkArticleEmitted(jobID, messageID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, err := q.residentJob(jobID)
	if err != nil {
		return err
	}
	i, ok := job.manifest.articleIndexByID(messageID)
	if !ok {
		return fmt.Errorf("%w: article %s in job %s", ErrNotFound, messageID, jobID)
	}
	job.progress.markEmitted(job.manifest, i)
	return nil
}

// MarkArticleEmittedByIdx flags article artIdx as emitted in O(1) time.
func (q *Queue) MarkArticleEmittedByIdx(jobID string, artIdx int32) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	// Non-residency and a genuine bounds violation were previously reported
	// with the same "out of range" message, which misdiagnoses a de-hydrated
	// job as a caller bug. Keep them separate.
	job, err := q.residentJob(jobID)
	if err != nil {
		return err
	}
	if int(artIdx) < 0 || int(artIdx) >= job.manifest.NumArticles() {
		return fmt.Errorf("queue: artIdx %d out of range for job %s", artIdx, jobID)
	}
	job.progress.markEmitted(job.manifest, int(artIdx))
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
	// Skipping this for a non-resident job is safe: emitted is transient and
	// excluded from persistence, and the progress holding it is discarded on
	// eviction, so PromoteNext rebuilds it all-false from stored state and the
	// article is offered again.
	job, err := q.residentJob(jobID)
	if err != nil {
		return err
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

// ClearArticleEmittedByIdx resets the transient Emitted flag on article artIdx
// in O(1) time. Returns ErrNotFound if the job is absent, ErrJobNotResident if
// it exists but is not currently hydrated, and a plain error only for a genuine
// out-of-range artIdx.
func (q *Queue) ClearArticleEmittedByIdx(jobID string, artIdx int32) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	// As in MarkArticleEmittedByIdx: a non-resident job is not a bounds error.
	job, err := q.residentJob(jobID)
	if err != nil {
		return err
	}
	if int(artIdx) < 0 || int(artIdx) >= job.manifest.NumArticles() {
		return fmt.Errorf("queue: artIdx %d out of range for job %s", artIdx, jobID)
	}
	job.progress.clearEmitted(job.manifest, int(artIdx))
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
		m := job.manifest
		if m != nil && job.progress != nil {
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

// TotalRemainingBytes returns the sum of RemainingBytes across all jobs,
// including jobs that are not currently resident (only maxActive jobs have a
// resident Manifest at once). JobProgress is always resident regardless of
// manifest residency — see docs/queue-lifecycle.md — so every job's
// remainingBytes is a live read, never a cached fallback.
func (q *Queue) TotalRemainingBytes() int64 {
	q.mu.RLock()
	defer q.mu.RUnlock()
	var total int64
	for _, job := range q.byID {
		// The nil-safe accessor rather than the raw field, for the reason
		// residentJob's own guard gives: the sole production caller is
		// runMetricsPush, a 1 Hz ticker goroutine with no recover(), so a
		// nil progress here is process death within a second. The invariant
		// holds today — Add repairs a nil-progress job at the entry point —
		// but this costs one comparison to not depend on it.
		total += job.Progress().RemainingBytes()
	}
	return total
}

// CheckEarlyAbort checks whether the job should be aborted based on the
// first-article failure rate heuristic. Returns true exactly once per
// job when the threshold is exceeded.
func (q *Queue) CheckEarlyAbort(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	// A non-resident job has no live JobProgress, so there is no failure rate
	// to evaluate and nothing to abort. Match the not-found answer rather than
	// inventing a third outcome for a method with no error channel.
	job, err := q.residentJob(jobID)
	if err != nil {
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
	job, err := q.residentJob(jobID)
	if err != nil {
		q.mu.Unlock()
		return err
	}
	var notFound []string
	for _, id := range messageIDs {
		i, ok := job.manifest.articleIndexByID(id)
		if !ok {
			notFound = append(notFound, id)
			continue
		}
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

// MarkArticlesDoneByIdx is the O(1) index-based batched form of MarkArticlesDone.
func (q *Queue) MarkArticlesDoneByIdx(jobID string, artIdxs []int32) error {
	if len(artIdxs) == 0 {
		return nil
	}
	q.mu.Lock()
	job, err := q.residentJob(jobID)
	if err != nil {
		q.mu.Unlock()
		return err
	}
	nArt := job.manifest.NumArticles()
	var invalidCount int
	for _, idx := range artIdxs {
		i := int(idx)
		if i >= 0 && i < nArt {
			job.progress.markDone(job.manifest, i)
		} else {
			invalidCount++
		}
	}
	q.dirty.Store(true)
	q.mu.Unlock()
	if invalidCount > 0 {
		q.log.Warn("MarkArticlesDoneByIdx: out-of-bounds article index received", "job", jobID, "invalid_count", invalidCount, "num_articles", nArt)
	}
	return nil
}

// SetFileWriteCursor records the assembler's contiguous write frontier for a
// file as a persisted resume hint (see JobFile.WriteCursor). Called from the
// assembler's batched flush, never per-article.
func (q *Queue) SetFileWriteCursor(jobID string, fileIdx int, cursor int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, err := q.residentJob(jobID)
	if err != nil {
		return err
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
	job, err := q.residentJob(jobID)
	if err != nil {
		q.mu.Unlock()
		return nil, err
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

// MarkArticlesFailedByIdx is the O(1) index-based batched form of MarkArticlesFailed.
func (q *Queue) MarkArticlesFailedByIdx(jobID string, artIdxs []int32) ([]int32, error) {
	if len(artIdxs) == 0 {
		return nil, nil
	}
	q.mu.Lock()
	job, err := q.residentJob(jobID)
	if err != nil {
		q.mu.Unlock()
		return nil, err
	}
	nArt := job.manifest.NumArticles()
	firstTime := make([]int32, 0, len(artIdxs))
	var invalidCount int
	for _, idx := range artIdxs {
		i := int(idx)
		if i >= 0 && i < nArt {
			if job.progress.markFailed(job.manifest, i) {
				firstTime = append(firstTime, idx)
			}
		} else {
			invalidCount++
		}
	}
	var failedBytes, par2Bytes int64
	var releasedPar2 bool
	if len(firstTime) > 0 {
		failedBytes, par2Bytes = job.progress.failedBytes, job.manifest.Par2Bytes()
		q.dirty.Store(true)
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
	if invalidCount > 0 {
		q.log.Warn("MarkArticlesFailedByIdx: out-of-bounds article index received", "job", jobID, "invalid_count", invalidCount, "num_articles", nArt)
	}
	if len(firstTime) > 0 {
		q.log.Warn("articles marked FAILED by idx", "job", jobID, "count", len(firstTime), "failed_bytes", failedBytes, "par2_bytes", par2Bytes)
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
	job, err := q.residentJob(jobID)
	if err != nil {
		return err
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
	// Progress tier: the reason is a plain string on JobProgress, which is
	// permanently resident, so this neither needs the manifest nor can fail
	// on residency. It used to require both and return nil when either was
	// absent, which meant the reason for releasing a job's par2 volumes was
	// silently dropped for exactly the non-resident jobs the on-demand par2
	// path operates on.
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
	job, err := q.residentJob(jobID)
	if err != nil {
		return err
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
		// bitset has no slice/append primitive (see bitset.go), so surviving
		// articles are copied bit-by-bit into pre-sized bitsets via a running
		// index, rather than the []bool append-of-range this replaces.
		newDone := newBitset(newManifestVal.NumArticles())
		newFailed := newBitset(newManifestVal.NumArticles())
		newEmitted := newBitset(newManifestVal.NumArticles())
		newFiles := make([]FileProgress, 0, newManifestVal.NumFiles())
		idx := 0
		for fi := range m.NumFiles() {
			if job.progress.files[fi].Deferred {
				continue
			}
			// A Deferred file's articles are never dispatched
			// (ForEachUnfinishedArticle skips them), so they are always
			// Done=false/Failed=false/Emitted=false at discard time —
			// dropping them loses nothing.
			lo, hi := m.FileRange(fi)
			for i := lo; i < hi; i++ {
				if job.progress.done.Get(i) {
					newDone.Set(idx)
				}
				if job.progress.failed.Get(i) {
					newFailed.Set(idx)
				}
				if job.progress.emitted.Get(i) {
					newEmitted.Set(idx)
				}
				idx++
			}
			newFiles = append(newFiles, job.progress.files[fi])
		}
		// idx is expected to land exactly on newManifestVal.NumArticles():
		// both loops above walk the identical Deferred predicate in the
		// identical file order, so every surviving article should get
		// exactly one slot. That equality is not self-enforcing, though —
		// bitset.Set silently no-ops out of range (see bitset.go), so a
		// future divergence between the two loops would corrupt done/
		// failed/emitted by silently dropping or misplacing bits instead of
		// failing loudly. Check it explicitly rather than trust it.
		if idx != newManifestVal.NumArticles() {
			panic(fmt.Sprintf("queue: DiscardDeferredPar2 copied %d articles but the rebuilt manifest has %d — the surviving-file walk and the manifest's Deferred filtering have diverged", idx, newManifestVal.NumArticles()))
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

		job.setResidency(newManifestVal, newProgress)
		// The rebuilt manifest has fewer files/articles/bytes than the one
		// the job's scalars were last synced from (the whole point of this
		// discard) — par2Bytes/par2Files are carried over unchanged above,
		// but totalBytes/numFiles/numArticles shrink, so the cached scalars
		// must be re-synced or TotalBytes()/NumFiles()/NumArticles() would
		// keep reporting the pre-discard totals forever.
		job.setScalarsFromManifest(newManifestVal)
		// Set before the store write, not after: the in-memory job has already
		// changed at this point, so it is dirty whether or not persisting it
		// succeeds.
		q.dirty.Store(true)

		// Persist the rebuilt shape before releasing the lock (#294). Holding
		// q.mu across the store write is the same trade Add makes and is
		// justified the same way: the in-memory job and the persisted one must
		// never be observable at different file sets. Releasing the lock first
		// would open a window where a concurrent Snapshot reads the shrunk
		// progress, hydrates the un-rewritten manifest, and gets ErrManifestStale
		// — reintroducing the bug for as long as the window lasts.
		//
		// A failure here is reported rather than swallowed, and the in-memory
		// rebuild above is deliberately left in place: it is correct, it is
		// what every reader in this process will see, and rolling it back
		// would re-defer volumes the caller has already verified as
		// unnecessary.
		//
		// What that leaves behind is not benign, and an earlier version of
		// this comment claimed it was — that a restart merely resurrects the
		// volumes, no worse than the pre-#294 behaviour. It is worse: the
		// rows still describe the pre-discard file set while the manifest
		// describes the new one, and updateTx writes job_files by
		// file_index, so every checkpoint tick would splice each surviving
		// file's progress onto its pre-discard neighbour's row (#310).
		//
		// Flag the job instead. The flag is raised before the attempt, not
		// after a failure, so a panic or a partially-applied write inside
		// ReplaceManifest leaves it raised too — the states this guards
		// against are exactly the ones where control does not come back
		// here cleanly.
		// Bump before the store write, so a rewrite already in flight for
		// the previous file set cannot clear the flag this one is about to
		// raise.
		job.bumpFileSetGen()
		if q.store != nil {
			job.setManifestRowsStale(true)
			if err := q.store.ReplaceManifest(context.Background(), job); err != nil { //lockio: the in-memory and persisted file sets must not diverge; see comment above
				return fmt.Errorf("queue: persist discarded par2 for job %s: %w", jobID, err)
			}
			job.setManifestRowsStale(false)
		}
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
	job, err := q.residentJob(jobID)
	if err != nil {
		return err
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
	job, err := q.residentJob(jobID)
	if err != nil {
		return err
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
	job, err := q.residentJob(jobID)
	if err != nil {
		return err
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

// ResumeAll clears the queue-wide pause flag, marks the queue dirty,
// and triggers the promotion loop to activate pending jobs.
func (q *Queue) ResumeAll(ctx context.Context) {
	q.mu.Lock()
	q.paused = false
	q.dirty.Store(true)
	q.notifyLocked()
	// Lock-ordering invariant: q.mu MUST be unlocked before calling PromoteNext.
	// PromoteNext performs disk I/O to read gzipped article manifests for newly promoted
	// active jobs; holding q.mu during disk I/O would block queue status queries.
	q.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	q.PromoteNext(ctx)
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
