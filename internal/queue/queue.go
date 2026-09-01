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
	"github.com/hobeone/gonzbd/internal/durability"
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
// runs on background goroutines that carry no recover.
//
// No write path this process performs can produce the mismatch any more. It
// had one ordinary cause until #294 — DiscardDeferredPar2 rebuilding a
// smaller manifest without rewriting the blob — and one residual cause after
// that, a torn Store.ReplaceManifest whose blob write and transaction could
// not be made atomic. Both are gone: the file set is now immutable after Add,
// which writes the blob before opening the transaction, so a crash between
// them leaves an orphan manifest and no job row rather than a disagreeing
// pair.
//
// What this guard actually checks is manifest-versus-progress size
// agreement — NumFiles/NumArticles against len(progress.files)/done.Len() —
// so what it can detect is a truncated or damaged manifest blob. It cannot
// detect job_files rows altered out of band: RestoreJobProgress fills
// progress.files by file_index under a bounds check and never resizes it,
// so rows deleted or renumbered outside this process pass this guard
// silently and land per-article state on the wrong file's slot instead of
// raising this error. Reporting the mismatches this can see is still worth
// doing; the alternative is not "no error" but a panic on a goroutine with
// no recover, which is strictly worse for the same underlying state.
//
// The boot path (SQLiteStore.Get) carries no guard at all — it sizes
// progress from the manifest it reads and fills it by file_index with no
// describesSameJobAs check, so a manifest/job_files disagreement at startup
// is undetected either way. What to do about that is #278, open.
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

	// dirty is set to true by the article/file mutation methods
	// (AckDurable, AckPermanentFailure, MarkFileComplete, SeedFromRuns,
	// ReplaceFromRuns) and cleared by Save on a
	// successful write. The periodic checkpoint ticker no-ops when
	// dirty is false, avoiding unnecessary I/O on idle queues.
	dirty atomic.Bool

	// failedPersistMu serialises every failed_articles STORE call against
	// every reversal of one, and failedGen makes that serialisation ordered
	// rather than merely exclusive.
	//
	// The INSERT happens outside mu — the project bans I/O under a lock — so
	// exclusion alone does not settle the outcome. AckPermanentFailure marks
	// an article failed under mu and INSERTs after releasing it. Of the two
	// reversals, ClearAllEmitted matches that shape: it clears the bits under
	// mu and DELETEs after releasing it. Queue.Retry does NOT — it deletes
	// while still holding mu, which is why the LOCK ORDER note below exists
	// at all. Interleaved,
	// the INSERT can land after the DELETE and leave a row with no in-memory
	// bit behind it, which the next RestoreJobProgress reads back as
	// Failed+Done. The article is then never re-fetched. This is not a
	// theoretical window: ClearAllEmitted exists for articles failed during
	// the old downloader's teardown, so acks in flight across a reload are
	// precisely its designed-for case.
	//
	// failedGen is bumped under mu by every reversal, and an ack captures it
	// under mu alongside its in-memory mark. Under failedPersistMu the ack
	// re-reads it and skips the INSERT if it moved, so a write whose bit has
	// since been cleared cannot be committed. The mutex is what makes that
	// check meaningful — without it the DELETE could still overtake an INSERT
	// that passed the check.
	//
	// One counter for the whole queue rather than one per job. A reversal of
	// job A therefore discards an in-flight ack for job B as well, which
	// costs one re-request of an article that fails again — R10's direction,
	// and the same trade AckPermanentFailure's best-effort persist already
	// takes.
	//
	// LOCK ORDER: mu before failedPersistMu. Queue.Retry deletes while
	// holding mu and so takes both; nothing takes mu while holding
	// failedPersistMu.
	//
	// That nesting has a COST worth naming rather than discovering: Retry
	// blocks on failedPersistMu with mu held, so an AckPermanentFailure whose
	// INSERT is slow — a stalled disk, a locked database — holds the queue
	// lock shut for the duration of that INSERT, and every reader of the
	// queue waits behind it. It is accepted rather than overlooked. Doing the
	// delete after releasing mu would put it in the same shape as
	// ClearAllEmitted's, but Retry's delete is ordered against an Update in
	// the same critical section (see Queue.Retry), and splitting the two is
	// what would leave a stale failed row beside a reset job.
	failedPersistMu sync.Mutex
	failedGen       atomic.Uint64

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
//
// The consequence, since this hands the channel across a package boundary:
// Downloader.run selects on it in a loop and its arm does not exit, so a
// closed notifyCh would be permanently ready and would spin that loop at full
// CPU. Never close it — see docs/go-standards.md § Concurrency & Locking.
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
// This is now a property of the whole package, not just the direction of
// travel: every manifest-tier mutation passes a residency gate, so none of
// them can report success for work it did not do (#261). The gate is no
// longer this function alone. B2.4a moved the manifest-tier bodies onto *Job,
// and each one now opens with Job.resident; the *Queue wrappers that call
// them look their job up in q.byID directly. AckPermanentFailure and
// ReplaceFromRuns still start here, because they need the manifest in the
// wrapper itself rather than only inside the moved body.
//
// CheckEarlyAbort is the third caller, and the one exception to the rule
// below: `git grep -n 'q\.residentJob(' -- '*.go' ':!*_test.go'` returns 3.
// It needs no manifest, so by the rule it should not be here — but residency
// coincides exactly with the condition under which its answer can be acted
// on, and #461 was filed and closed proving that. See its own doc comment for
// the argument; what matters here is that its reason is actionability, never
// the absence of progress, and that the rule below is otherwise unqualified.
//
// Which of the two entry points a given method uses is not the invariant and
// should not be asserted method by method — that is enforced rather than
// asserted, because hand searches over this surface have three times now
// returned a different subset of it. TestManifestAccessIsGated walks the
// package AST and fails any *Queue or *Job method that dereferences a
// manifest without calling this or Job.resident.
//
// The condition itself lives on Job.resident, which this delegates to; the
// two are one gate with two entry points, not two gates. This one is for a
// caller that starts from an ID and adds the ID to the error, that one for a
// caller that already holds the *Job.
//
// The progress-tier methods do not route through here, CheckEarlyAbort
// excepted. JobProgress is permanently resident, so gating them on residency
// would refuse work they are always able to perform, and doing it because
// "a non-resident job has no progress" is a bug in the same family as the one
// SetPar2ReleaseReason had — it silently dropped the reason a job's par2
// volumes were released for every non-resident job.
//
// CheckEarlyAbort keeps its gate on a different argument entirely: not that it
// cannot compute an answer, but that no consumer could act on one. Requiring
// that second argument is the point — a gate justified by the first is wrong,
// and #461 is the worked example of the first reading being written down and
// then believed.
//
// Manifest is the only residency signal now: JobProgress is permanently
// resident (docs/queue-lifecycle.md) and never nil for a job in q.byID, so a
// non-resident job is manifest-nil/progress-live, not both-nil — that state
// is the ordinary steady state for every StatusQueued/StatusPaused job, not
// a half-hydration hazard. Skipping work on ErrJobNotResident is still safe,
// for a reason that no longer depends on progress's presence: every MUTATING
// caller through this gate needs the manifest itself to resolve what it was
// asked to mutate (a message ID or article index only means something against
// a resident Manifest), so there is no correct mutation to perform without
// one. Note what this argument turns on: not that the caller does not write,
// but that what it writes is INDEXED BY THE MANIFEST.
//
// And should a caller somehow bypass that, whatever it wrote would not survive
// promotion. Which function discards it depends on the path, and this comment
// named the wrong one until #461's review: hydrateJobLocked PRESERVES the live
// JobProgress — that is #276, and its "Keep the progress the job already
// carries" block says so — while PromoteNext's own loop replaces it outright,
// `job.setResidency(&manifest, newJobProgress(&manifest))`, before
// Store.RestoreJobProgress refills the counters from persisted state. Two
// paths, one preserving and one rebuilding, which is why a claim about "the
// moment the job returns to resident" has to name which. The skipped mutation
// is moot on the PromoteNext path because promotion supersedes it, not
// because progress was about to vanish.
//
// The job.progress == nil clause, now on Job.resident, is not load-bearing
// for any known caller — no code path leaves a job in q.byID with nil
// progress — but it stays as defense in depth: everything downstream of this gate assumes
// job.progress is safe to dereference, and every worker goroutine that could
// hit a violation carries no recover(), so the cost of keeping a redundant
// check is a single comparison against the cost of a process crash if that
// invariant is ever broken elsewhere.
func (q *Queue) residentJob(id string) (*Job, error) {
	job, ok := q.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := job.resident(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, id)
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
	job, ok := q.byID[jobID]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	return job.countUnfinishedArticles(fileIdx)
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
	// reopen the exact race this ordering exists to close: Queue.SnapshotJob
	// clones a job under RLock and hydrates it from disk after releasing the
	// lock, so an unlink that races ahead of the job's removal from q.byID
	// can catch a snapshot mid-hydration.
	//
	// SnapshotJob, not Snapshot: the plural form stopped hydrating and says
	// so at its own declaration.
	//
	// SnapshotJob is the only reader this ordering PROTECTS, not the only one
	// that reads a manifest unlocked — PromoteNext does too, at the Step 2
	// read below, and defends itself instead by re-checking q.byID after
	// re-acquiring the lock and skipping a job that vanished meanwhile.
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
// stale on-disk row and silently undo the reset one layer down (the exact
// failure mode issue #260 describes). The job's failed_articles rows are
// dropped in the same window and for the same reason: nothing else rewrites
// them, so a stale one would come back as a failed article the retry never
// re-attempts.
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
			// ResetForRetry cleared every failed bit in memory; the stored
			// rows have to go with them. A failed_articles row is not
			// re-serialised by the Update below the way the articles_done
			// blob it replaced was, so one left behind survives the retry and
			// the next restart marks the retry's re-fetched article failed.
			//
			// Before the Update and inside the same failure branch: if this
			// cannot be made durable the retry must roll back, for exactly the
			// reason #260 gives about the row itself. Scoped to this one job —
			// see SQLiteStore.ClearFailedArticles for what a wider sweep would
			// resurrect.
			//
			// The order leaves ONE asymmetry, and it is the safe one. If this
			// succeeds and the Update below then fails, the rollback restores
			// the failed bits in memory while the stored rows are already
			// gone: a restart before the next retry derives those articles as
			// Outstanding and re-attempts them. That is R10's direction —
			// losing a permanent failure costs one request, and the article
			// fails again. Reversing the two would leave a STALE failed row
			// beside a reset job, which is the direction this whole step
			// exists to prevent.
			//
			// The generation bump and failedPersistMu are the same pairing
			// ClearAllEmitted uses, and for the same reason: an
			// AckPermanentFailure for this job may have released q.mu with
			// its INSERT still pending, and without both it could land after
			// this delete. Taking failedPersistMu while holding q.mu is the
			// declared lock order (see Queue's failedPersistMu doc); nothing
			// acquires q.mu while holding it.
			q.failedGen.Add(1)
			q.failedPersistMu.Lock()
			clearErr := q.store.ClearFailedArticles(context.Background(), id) //lockio: drops the retry's stale failed rows before PromoteNext's unconditional RestoreJobProgress can re-read them
			q.failedPersistMu.Unlock()
			if clearErr != nil {
				job.Status, job.Warning = prevStatus, prevWarning
				job.setResidency(job.manifest, prevProgress)
				q.evictJobLocked(job)
				q.mu.Unlock()
				return fmt.Errorf("queue: retry job %s: clear failed articles: %w", id, clearErr)
			}
			if err := q.store.Update(context.Background(), job); err != nil { //lockio: persists the reset progress before PromoteNext's unconditional RestoreJobProgress can re-read the stale row
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
				cf := q.prepareClaimFailureLocked(job, manifestPath, fmt.Errorf("restore progress: %w", err))
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
	job.Warning = fmt.Sprintf("Corrupt manifest: %v", claimErr)
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
			FailMessage: fmt.Sprintf("Unreadable or corrupt manifest: %v", claimErr),
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
	job.setPP(pp)
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
	job.setName(name)
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
	job.setScript(script)
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
	// fetch_policy column; without one it was the original bug on the live
	// job.
	//
	// The same staleness hazard applies, so the same guard does. A manifest
	// read from disk can predate a manifest write that has since gone
	// missing or been truncated, and pairing a mismatched manifest/progress
	// pair panics inside recompute. Fail closed rather than crash: this path
	// already fails closed for an unreadable manifest, and a manifest that
	// contradicts the job is no more usable than one that will not parse.
	// DiscardDeferredPar2 no longer shrinks anything — a discard moves a
	// file's fetch_policy in place and never touches the file set (see its
	// doc comment) — so it is not a source of this mismatch any more.
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
	// The bool is discarded on purpose: a job whose finish was already marked
	// is the ordinary case here, not an error. Before #464 this site applied
	// its own IsZero test to the same field; delegating is what stops the two
	// copies of first-wins from drifting.
	job.progress.setDownloadFinishedOnce(time.Now().UTC())
	if q.store != nil {
		_ = q.store.Update(context.Background(), job) //lockio: keeps RAM and SQLite views of the PostProc transition consistent; tracked in #229
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
	// Progress tier: no residency guard. The first-finish-wins test is a
	// business condition, not a check for absence — progress is permanently
	// resident — and since #464 it lives on JobProgress with the field it
	// guards, which markDownloadFinishedOnce delegates to.
	if job.markDownloadFinishedOnce(t) {
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
	// Progress tier: see MarkDownloadFinished. "First start wins" is a
	// business condition, not a residency check.
	if job.markStartedOnce(t) {
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
	job.recordDownload(server, bytes)
	q.dirty.Store(true)
	return nil
}

// UnfinishedArticle is the snapshot record yielded by
// ForEachUnfinishedArticle. It carries the minimum the dispatcher
// needs to target a specific article; full Job state stays behind
// the queue's lock.
type UnfinishedArticle struct {
	JobID     string
	JobStatus constants.Status
	JobAdded  time.Time
	FileIdx   int
	ArtIdx    int32 // Global article index within Manifest
	MessageID string
	Bytes     int
	Subject   string
	// PartNumber is the NZB segment number for this article — what the
	// indexer says this part's ordinal is. Carried per-article on the same
	// terms as Bytes and Subject.
	//
	// The dispatcher compares it against the ordinal the SERVER declares in
	// =ybegin part= and counts disagreements (#379). Nothing acts on it. It is
	// the first consumer Manifest.ArticleNumber has ever had, which is
	// precisely why the comparison only counts: an unvalidated field on both
	// sides is not something to start failing articles on.
	PartNumber int
	// RepairState is the job's repairability verdict, derived once per job in
	// the walk below and carried per-article so the dispatcher's Early Health
	// Gate can read it without reaching back into the Job.
	//
	// It carries the verdict rather than the three figures behind it because
	// the dispatcher re-deriving the comparison is how the gate drifted from
	// failMsgForJob twice — and a struct field is invisible to a reference
	// search on the accessors, so the drift did not surface either time. Ask
	// RepairState.Hopeless(); do not reconstruct the comparison here.
	RepairState RepairState
}

// ForEachUnfinishedArticle invokes fn for every not-yet-Done article
// in the queue, in priority/file/article order. The read lock is
// held across the whole iteration — fn must not call back into the
// Queue (e.g. Add, Remove, AckDurable) or it will deadlock.
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
		// Hoisted out of the per-article loop: both are walks over files and
		// do not change while this job's articles are enumerated.
		// Derived from the manifest rather than through Job.RepairState():
		// this walk runs under q.mu and the Job accessors take residencyMu,
		// and the manifest figures are already in hand here.
		repairState := RepairStateFrom(
			job.progress.ContentFailedBytes(),
			m.RecoveryBytes(),
			job.progress.HasPar2Files(),
		)
		for fi := range m.NumFiles() {
			// Files that are not being fetched (on-demand par2 recovery
			// volumes, held or discarded) already have Pending == 0 from
			// recompute, so the next check skips them too; the explicit
			// guard documents intent and protects against counter drift.
			if job.progress.files[fi].Complete || job.progress.files[fi].Pending == 0 || job.progress.files[fi].Fetch != FetchAlways {
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
					PartNumber:  m.ArticleNumber(i),
					RepairState: repairState,
				}) {
					return
				}
			}
		}
	}
}

// MarkArticleEmittedByIdx flags article artIdx as having a result in flight
// from the downloader to the assembler. This is a transient, in-memory-only
// bit (JobProgress's emitted array, never persisted): its purpose is to
// prevent the dispatcher from re-dispatching the same article between the
// moment the downloader sends a result on the completions channel and the
// moment the outcome is resolved — by durability.Barrier through AckDurable
// for a success, or by internal/app's pipeline through AckPermanentFailure for
// a permanent failure. Neither comes from the assembler, which has no ack path
// in either direction. On restart the flag is lost, so any article no barrier
// made durable is re-downloaded; see docs/nntp-downloader-contract.md §5.
//
// Idempotent: setting Emitted on an article that is already Emitted, Done,
// or Failed is a no-op. Returns ErrNotFound if the job is absent,
// ErrJobNotResident if it exists but is not currently hydrated, and a plain
// error only for a genuine out-of-range artIdx.
func (q *Queue) MarkArticleEmittedByIdx(jobID string, artIdx int32) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	return job.markArticleEmittedByIdx(artIdx)
}

// ClearArticleEmittedByIdx resets the transient Emitted flag on article artIdx,
// allowing the dispatcher to re-dispatch it. This is used when the pipeline
// receives a retryable download error: the article was emitted but did not
// succeed on this attempt, so it must be returned to the dispatch pool.
// Wakes the dispatcher so the article is picked up on the next pass.
//
// See MarkArticleEmittedByIdx for what the Emitted bit means and why it is
// transient. Errors follow the same three cases it documents.
func (q *Queue) ClearArticleEmittedByIdx(jobID string, artIdx int32) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	// As in MarkArticleEmittedByIdx, a non-resident job is not a bounds error —
	// and returning early for one is safe: emitted is transient and excluded
	// from persistence, and the progress holding it is discarded on eviction,
	// so PromoteNext rebuilds it all-false from stored state and the article is
	// offered again.
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	if err := job.clearArticleEmittedByIdx(artIdx); err != nil {
		return err
	}
	q.notifyLocked()
	return nil
}

// ClearAllEmitted resets transient article state for a downloader reload.
// This must be called when the downloader is reloaded: articles that were
// Emitted by the old downloader but never completed (because the old
// downloader was stopped) would otherwise be permanently skipped by
// ForEachUnfinishedArticle.
//
// skipJobIDs names the jobs whose Emitted bits must be LEFT SET, because the
// caller's durability checkpoint could not make their written articles durable.
// Clearing those would offer bytes that are already on disk for re-fetch, and
// a re-fetch that fails against a changed server set marks the article
// permanently failed while its data sits there — inflating failedBytes far
// enough to abort a job whose file was never damaged (#417).
//
// The skip is narrow on purpose: it withholds the Emitted clear and nothing
// else. A skipped job still has its teardown-failed articles un-failed, still
// recomputes, and still takes part in the stored-row reconciliation, because
// those act on articles that are failed-and-not-emitted — a disjoint set. See
// resetForReload for why that disjointness holds.
//
// Pass nil to clear everything, which is right at startup: nothing is in
// flight, and the resume sweep re-derives from the durability record
// immediately afterwards.
//
// Additionally, articles marked Failed during the old downloader's
// teardown (e.g. from context cancellation) are reset to retryable
// state. Without this, a late assembler flush could race ahead and
// permanently mark articles as Done+Failed, preventing re-dispatch.
//
// Two kinds of article keep their resolution. A successfully completed one
// (Done=true, Failed=false) is finished. A failed one whose file is already
// Complete has nowhere to be retried to — the file is finalized, truncated and
// tombstoned in the assembler — so resetting it would count work that
// ForEachUnfinishedArticle then skips forever (#426). JobProgress.resetForReload
// owns that decision and reports which articles it cleared, because the stored
// failed_articles rows must be dropped for exactly those and no others.
func (q *Queue) ClearAllEmitted(skipJobIDs map[string]struct{}) {
	q.mu.Lock()
	// reset collects the jobs whose failed articles this call cleared in
	// memory, so their stored rows can be dropped below the lock. A
	// failed_articles row is not re-serialised by the next job update the way
	// the articles_done blob it replaced was, so a row left behind survives
	// the reload and the next restart marks the re-attempted article failed
	// again.
	// arts are the articles whose failed bit this call CLEARED, whose rows
	// must go. retained are the ones it deliberately left failed — an article
	// on a Complete file — whose rows must stay, and which the generation bump
	// below can cost an in-flight ack. See the reconciling write.
	type jobReset struct {
		id       string
		arts     []int32
		retained []int32
	}
	var reset []jobReset
	var anyCleared bool
	for _, job := range q.jobs {
		m := job.manifest
		if m != nil && job.progress != nil {
			// A job the caller's checkpoint could not make durable keeps its
			// Emitted bits — and ONLY those. Everything else in this loop
			// still runs for it, because the un-failing below acts on a
			// disjoint set of articles and withholding it would strand the
			// old downloader's teardown failures permanently (#417).
			_, skipEmitted := skipJobIDs[job.ID]
			var cleared, retained []int32
			for i := range m.NumArticles() {
				// Reset articles that were marked Failed during the old
				// downloader's teardown so they can be retried by the
				// new downloader. Two kinds are left untouched:
				// successfully completed articles (Done && !Failed), and a
				// failed article whose file is already Complete, which has
				// nowhere to land (#426 — see resetForReload).
				switch {
				case job.progress.resetForReload(m, i, !skipEmitted):
					cleared = append(cleared, int32(i))
				case job.progress.failed.Get(i):
					retained = append(retained, int32(i))
				}
			}
			// Only jobs with something to do below. A job that neither
			// cleared nor retained anything has no rows to drop and none to
			// re-assert, and carrying it here would buy a store round trip
			// per resident job on every reload.
			if len(cleared) > 0 || len(retained) > 0 {
				reset = append(reset, jobReset{id: job.ID, arts: cleared, retained: retained})
			}
			anyCleared = anyCleared || len(cleared) > 0
			// Recompute pending counters from ground truth after bulk
			// state reset. Incremental tracking is fragile here because
			// both Emitted and Failed flags are being cleared in bulk.
			job.progress.recompute(m)
		}
	}
	// Bumped under mu, in the same hold that cleared the bits, so any
	// AckPermanentFailure that captured the previous value has already made
	// its in-memory mark and will find its own write invalidated below. See
	// the failedPersistMu/failedGen doc on Queue.
	//
	// Only when something was actually cleared. The bump's meaning is "a
	// reversal has invalidated pending writes", and a reload that reversed
	// nothing has invalidated nothing — bumping anyway drops the insert of
	// every ack in flight across the whole queue and loses a permanent failure
	// that no reversal ever touched.
	if anyCleared {
		q.failedGen.Add(1)
	}
	// Drain any stale notification (e.g. from AckDurable calls during
	// pipeline drain of the old downloader's buffered results) so our fresh
	// notification is guaranteed to be delivered.
	select {
	case <-q.notifyCh:
	default:
	}
	q.notifyLocked()
	q.mu.Unlock()
	// --- No lock held below this line ---

	// One delete per RESIDENT job, naming the articles the loop above
	// actually reset. The scoping to resident jobs is still load-bearing — a
	// sweep over every job would resurrect the permanently failed articles of
	// every NON-resident job as fetchable work, and the symptom would be a
	// silent re-download storm rather than an error.
	//
	// What is no longer true is that a whole-job delete would do: since #426
	// the reset skips a failed article whose file is already Complete, so the
	// cleared set is a strict subset of the job's stored rows. Dropping the
	// rest would leave that article failed in memory and unrecorded on disk,
	// and the next restart would load it as outstanding work on a file
	// ForEachUnfinishedArticle skips — the same strand the guard exists to
	// prevent, one restart later.
	//
	// Best effort: this runs on a downloader reload, and a delete that fails
	// is logged rather than returned. The cost is not a wasted attempt — it is
	// the opposite. A row that outlives the clear is reloaded by
	// resolutionForJob and re-derives its article as Failed+Done, so the
	// article is NOT re-attempted after a restart, which is what the warning
	// below tells the operator.
	//
	// Under failedPersistMu, which is what stops an AckPermanentFailure that
	// unlocked mu before this call from landing its INSERT after these
	// deletes. Exclusion is only half of it — the generation bump above is
	// what tells that ack its rows are stale.
	//
	// # Why the retained articles are RE-asserted here
	//
	// The generation bump is queue-wide and all-or-nothing: an ack that
	// captured the old value drops its whole insert, whatever it was about.
	// That was exact while this loop reversed every failed article, because
	// then any row the ack meant to write had in fact just been reversed. It
	// is not exact now — an ack for an article on a Complete file is dropped
	// by a bump that some unrelated article's reset caused, and the article is
	// left failed in memory with no row, which is the strand this whole change
	// removes, arriving by another route.
	//
	// Making the drop precise is not available: the ack would have to re-read
	// the bits under mu while holding failedPersistMu, and the declared lock
	// order is mu BEFORE failedPersistMu (Queue.Retry takes the latter while
	// holding the former), so that inverts it.
	//
	// Asserting the rows here is exact instead, and it is the same
	// reconciliation the deletes above perform rather than a second authority
	// over the table. This call holds the truth: it read every bit under mu,
	// after any ack that the bump can invalidate had already made its mark. An
	// ack arriving LATER captures the bumped generation and writes normally.
	// RecordFailedArticles is INSERT OR IGNORE, so re-asserting a row that
	// survived costs nothing.
	//
	// Nothing to do when no job reversed or retained anything — the common
	// case, and it also means no bump happened, so no ack was invalidated.
	if q.store == nil || len(reset) == 0 {
		return
	}
	type clearFailure struct {
		id  string
		err error
	}
	var failures, retainFailures []clearFailure
	q.failedPersistMu.Lock()
	for _, r := range reset {
		if err := q.store.ClearFailedArticlesByIdx(context.Background(), r.id, r.arts); err != nil { //lockio: failedPersistMu exists to order exactly this delete against AckPermanentFailure's insert; see Queue.failedPersistMu
			failures = append(failures, clearFailure{id: r.id, err: err})
		}
		// After the delete, never before: the two sets are disjoint by
		// construction, but ordering the assert second means a bug that ever
		// made them overlap loses the row rather than resurrecting it, which
		// is R10's direction.
		if err := q.store.RecordFailedArticles(context.Background(), r.id, r.retained); err != nil { //lockio: failedPersistMu exists to order this re-assert against AckPermanentFailure's insert; see Queue.failedPersistMu
			retainFailures = append(retainFailures, clearFailure{id: r.id, err: err})
		}
	}
	q.failedPersistMu.Unlock()
	// Reported outside the mutex: it is held to order these writes against an
	// ack's insert, and a logging handler is neither of those.
	for _, f := range failures {
		q.log.Warn("could not clear a job's permanently failed articles on downloader reload; "+
			"they will not be re-attempted after a restart",
			"job", f.id, "err", f.err)
	}
	for _, f := range retainFailures {
		q.log.Warn("could not re-assert the failed articles a downloader reload kept; if an "+
			"ack was in flight for one, it may be lost and the article will reload as "+
			"outstanding work on a file that is already complete",
			"job", f.id, "err", f.err)
	}
}

// TotalRemainingBytes returns the sum of RemainingBytes across all jobs,
// including jobs that are not currently resident (only maxActive jobs have a
// resident Manifest at once). JobProgress is always resident regardless of
// manifest residency — see docs/queue-lifecycle.md — so every job's
// RemainingBytes() is a live read, derived from per-file state, never a
// cached fallback.
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
	// The residency gate here is the one place in this package where gating a
	// progress-tier read is correct, and #461 was filed to remove it on the
	// strength of a rationale that was false. Keeping it, with the true reason
	// stated, because the false one has now misled a reader once.
	//
	// NOT because a non-resident job has no failure rate to evaluate — that
	// was the old claim and it is wrong twice over: JobProgress is permanently
	// resident, so a non-resident job is manifest-nil/progress-live, and its
	// rate is right there to read.
	//
	// The real reason is that residency coincides with the only condition
	// under which an abort verdict means anything. IsResident is PhaseActive
	// or PhaseProcessing, so a non-resident job is Queued, Propagating, Paused
	// or terminal — none of which is downloading. There is no bandwidth to
	// save, and no legal status edge from any of them to StatusVerifying, so
	// the sole caller's response (internal/app/pipeline.go's onJobHopeless →
	// maybeFinalize → SetPostProcStarted) fails with
	// ErrIllegalStatusTransition and drops the abort without logging it.
	// Answering "true" there would emit a WARN saying the job was aborted
	// while nothing aborted it.
	//
	// The verdict is deferred, not discarded: PromoteNext rebuilds JobProgress
	// from newJobProgress plus the store's persisted counters, earlyAborted is
	// deliberately not persisted (see jobProgressJSON), and the failure rate
	// that would have fired the abort is re-derived from failed_articles — so
	// the check fires on the first failure after the job is downloading again,
	// which is when it can act.
	//
	// That deferral rides on the store, and says nothing about a queue without
	// one: PromoteNext's RestoreJobProgress call is guarded on q.store != nil,
	// and newJobProgress alone starts the counters at zero, so a store-less
	// queue loses the rate across the round-trip and the abort never re-fires.
	// Production has no such queue — `git grep -n 'WithStateDir(' -- '*.go'
	// ':!*_test.go' ':!internal/queue/queue.go'` returns 0, and app.go:344's
	// sole queue.Load passes WithStore whenever repo.DB() is non-nil, which
	// both cmd/gonzbd entry points guarantee by aborting startup if
	// history.Open fails. The store-less form is test scaffolding, so this
	// states the condition rather than claiming nothing is ever lost.
	// TestCheckEarlyAbort_NonResidentDefersRatherThanAborts pins both halves,
	// store-backed for exactly that reason.
	job, err := q.residentJob(jobID)
	if err != nil {
		return false
	}
	return job.IsEarlyAbort()
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
	if err := job.checkFileIdxs(fileIdxs); err != nil {
		return err
	}
	if job.undeferRecovery(fileIdxs) {
		q.dirty.Store(true)
		q.notifyLocked()
	}
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
	job.setPar2ReleaseReason(reason)
	q.dirty.Store(true)
	return nil
}

// SetWarning attaches a human-readable reason to a job.
//
// It exists for R27: a job that stalls on a storage fault must surface a
// reason the user can act on, and Warning is the field the API and the UI
// already render. "Paused" with nothing beside it is exactly the unactionable
// halt the requirement forbids.
//
// Header tier: Warning is a plain string on the Job itself, so this needs
// neither the manifest nor residency and cannot fail on either — which
// matters, because the condition it reports is most likely to arrive when the
// job is in an unusual state.
func (q *Queue) SetWarning(jobID, warning string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	job.setWarning(warning)
	q.dirty.Store(true)
	return nil
}

// DiscardDeferredPar2 records that every recovery volume still awaiting the
// CRC verdict will never be downloaded. The file set does not change: the
// manifest entries and job_files rows stay exactly where they are, and only
// the fetch policy moves.
//
// This used to rebuild the manifest without those files. Removing a non-final
// file renumbers every file_index after it, and job_files rows are keyed by
// that index, which is the root of #294, #308, #310, #315 and #317. The only
// purpose removal ever served was accounting, and both derived size figures
// already exclude a file that is not being fetched, so there is nothing left
// for it to correct.
//
// No store write of its own. For a resident job the ordinary checkpoint
// writes the new policy through updateTx like any other per-file state, and
// the operation cannot partially apply. For a non-resident job it cannot:
// updateTx gates its whole job_files loop on job.Manifest() succeeding, and
// an evicted job has none, so a discard there stays in-memory-only on
// JobProgress — the persisted job_files.fetch_policy for that job's rows is
// unchanged.
//
// That mark is then lost, not merely deferred, the next time the job is
// promoted: PromoteNext rebuilds JobProgress from scratch with
// newJobProgress when job.manifest is nil (see queue.go's promotion loop),
// discarding the in-memory FetchNever, and RestoreJobProgress then assigns
// the stale FetchIfNeeded straight from the row. There is no
// residency-independent per-file write path to make the mark survive
// promotion; building one is out of scope here. This is bounded and
// self-correcting rather than a data-loss bug: HasDeferredPar2 is true again
// once the mark is lost, so maybeReleaseRecoveryVolumes re-runs the CRC pass
// on the next completion event and re-discards. The cost is one redundant
// verification pass — no re-download, no data loss.
//
// Progress tier, like SetPar2ReleaseReason immediately above: the fetch
// policy lives on JobProgress, which is permanently resident, so this
// neither needs the manifest nor can fail on residency. It used to require
// the manifest (to walk and rebuild it) and so returned ErrJobNotResident
// for a job whose manifest had been evicted for exceeding MaxActiveJobs —
// silently leaving that job's recovery volumes FetchIfNeeded, forcing
// maybeReleaseRecoveryVolumes to redo full CRC verification on every later
// completion event instead of trusting a verdict it already reached.
func (q *Queue) DiscardDeferredPar2(jobID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}

	if job.discardDeferredPar2() {
		q.dirty.Store(true)
	}
	return nil
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
	if err := job.markFileComplete(fileIdx); err != nil {
		return err
	}
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
	if err := job.setFileFilename(fileIdx, filename); err != nil {
		return err
	}
	q.dirty.Store(true)
	return nil
}

// SetFileCRC32FromRuns stores the assembled CRC32 on a JobFile, where
// QuickCheck reads it to verify file integrity against par2's file hashes
// without re-reading from disk — but only if the file's durable runs are
// evidence for it. Otherwise it records nothing and returns nil.
//
// It takes the RUNS rather than a uint32, and that is the point of its shape.
// The CRC's meaning is entirely a property of the record it came from, so a
// setter accepting a bare value has no way to refuse a wrong one and pushes
// the whole invariant into its caller's comments. Here there is nowhere else
// to write the field, and the evidence arrives with the claim.
//
// The value is the crc32 of the file's single durable run, not something the
// assembler computed. The assembler used to combine the per-article CRCs it
// happened to SEE, and a resumed run is never sent the articles an earlier run
// completed, so its parts did not tile the file and it generally could not
// produce one (#349). A run folds each article's CRC in as the article joins
// it, across restarts, so a resumed file supplies a CRC as readily as a fresh
// one.
//
// # The predicate: one run, at offset 0, covering every article of the file
//
// All three conditions, and the third is the one main used to enforce by other
// means. It is the direct expression of prefixWalk.consumedAll — "did the
// record account for every article of this file?" — in the vocabulary of runs,
// and #387 is what it is for.
//
// The geometric alternative — one row at offset 0 whose length equals the
// file's size — reads as the same rule and is not. It is INERT: FinalizeFile
// derives its truncate bound from max(offset+length) over the same rows, so
// size == Length holds by construction and the comparison can never fail.
// Worse, it is inert on exactly the case that matters. Two articles claiming
// one offset leave a single row at offset 0 after the merge drops one, its
// length equals the file's size, Σ length equals it too so no overlap is
// raised — and the CRC would be published over articles whose bytes another
// article has overwritten. par2.VerifyCRCs compares that value against the par2
// MANIFEST and never opens the file, so it MATCHES, QuickCheckClean is set, and
// the repair stage returns without running par2 at all.
//
// The article-coverage form closes that, and closes it exactly rather than
// heuristically. A dropped entry removes an article index from the record
// entirely; no other article carries that index, since ArtIdx is the manifest's
// unique global index; and a merge extends a span only to
// LastArtIdx+1, so a span never contains an index no article contributed. The
// dropped article is therefore in no run's span, and no single run can cover
// [lo, hi-1]. Every exact-offset collision fails this check.
//
// A file with a permanently failed article — INTERIOR or at the TAIL — also
// fails it, and unlike the row-count form this needs no exception. Its record
// does not account for every article, so no CRC is published, QuickCheck reads
// NoCRC, and the repair path runs. The tail case used to publish a CRC over
// the trimmed short file and rely on that value MISMATCHING par2 to reach the
// same place; it now reaches it one branch earlier, without a published number
// that describes bytes the file does not hold.
func (q *Queue) SetFileCRC32FromRuns(jobID string, fileIdx int, runs []durability.Run) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, jobID)
	}
	stored, err := job.setFileCRC32FromRuns(fileIdx, runs)
	if err != nil {
		return err
	}
	if stored {
		q.dirty.Store(true)
	}
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
