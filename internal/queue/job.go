// Package queue implements the in-memory job queue and its on-disk
// persistence.
//
// The queue is the central coordination point for the daemon: the HTTP
// layer calls Add/Remove/Pause/Resume; the downloader selects over
// Notify() and reads jobs in order via List(); the persistence layer
// serialises state to the admin directory so a restart can recover in
// flight downloads.
//
// # Concurrency model
//
// All public methods on *Queue are safe for concurrent use. Read-heavy
// operations (List, Len, Get) take the read lock; structural mutations
// (Add, Remove, Reorder, status changes) take the write lock.
//
// Jobs returned from List/Get are shared references into the Queue's
// internal storage. Job.Manifest() and Job.Progress() are safe to call
// without the queue lock — each takes the Job's own residency lock to
// synchronize against lazy eviction/hydration reassigning which
// Manifest/JobProgress the Job currently points to. That lock does NOT
// extend to the returned objects' contents: a Manifest is immutable after
// construction so nothing more is needed, but a JobProgress's fields are
// mutated in place and must only be changed through Queue methods that
// hold the write lock — direct mutation by callers, or reading through the
// pointer while such a mutation is in flight, is a data race.
//
// # Persistence
//
// Queue state lives in SQLite, through the Store supplied by WithStore:
// Save writes every job via Store.UpdateBatch and Load reads them back with
// Store.List. A job's immutable article structure is the exception — it is
// too large to want in a row and never changes after parse — so it is
// written once to manifests/<id>.json.gz, gzipped JSON via the same atomic
// temp+fsync+rename pattern the config package uses, and still readable
// with `zcat … | jq`.
//
// A Queue built without a Store persists nothing; it is an in-memory queue.
// Until #266 that configuration fell through to a second, whole-queue
// gzip-JSON engine (queue.json.gz plus jobs/<id>.json.gz) that no
// production path could reach. It has been removed.
package queue

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// Job is a live download job. It starts life when NewJob parses an NZB
// and ends when the downloader+postproc pipeline moves it to history.
//
// manifest/progress are unexported: external packages reach them only
// through the Manifest()/Progress() getters, which take residencyMu to
// synchronize the pointer read against concurrent reassignment (see
// residencyMu's doc comment). Neither Manifest nor JobProgress has a
// mutating exported method, so external packages cannot introduce new
// races on top of that — but see Progress()'s doc comment for what is
// still unguarded.
type Job struct {
	// ID is a 16-character lowercase hex string produced from 8 bytes
	// of crypto/rand output. Stable for the life of the job.
	ID string

	// Filename is the original NZB filename as supplied to Add. May be
	// empty when the caller had no filename (e.g. URL-grabbed NZBs
	// before the server provided a Content-Disposition).
	Filename string

	// NZBBackup is the basename of the gzipped NZB this job was backed up
	// to under admin/nzb/. Usually Filename + ".gz", but a forced duplicate
	// add takes a ".1"/".2" suffix rather than overwrite an existing
	// backup, so the two can diverge. Retry re-parses this file to recover
	// the article message-IDs, which live nowhere else once the job's
	// manifest is unlinked at finalization. Empty for jobs added before
	// the backup became load-bearing, or when the add supplied no filename.
	NZBBackup string

	// Name is the display name. Defaults to Filename minus extension;
	// callers can override via AddOptions.Name.
	Name string

	// Password is the archive password extracted from the filename or
	// supplied by the user. Empty if the job is unencrypted.
	Password string //nolint:gosec // G117: NZB archive password, not a credential

	// URL is the origin URL for URL-grabbed NZBs; empty for uploaded
	// or watched-dir NZBs.
	URL string

	// Category is the configured category name this job belongs to.
	// Resolved against the config's Categories list at download time.
	Category string

	// Priority is the user-selected priority. Queue ordering is driven
	// by this field at Add time; see insertByPriority.
	Priority constants.Priority

	// Status is the current lifecycle state. The queue manages
	// transitions between Queued and Paused; other states are driven
	// by the downloader and post-proc pipeline.
	Status constants.Status

	// PP is the post-proc level 0-3 (download / +unpack / +repair / +delete).
	PP int

	// Script is the name of an optional user post-proc script.
	Script string

	// Added is the wall-clock time when the job entered the queue.
	Added time.Time

	// Meta carries <meta> tags parsed from the NZB, preserved as a
	// slice-per-key to match the Python parser's multi-value semantics.
	Meta map[string][]string

	// Groups is the de-duplicated union of newsgroups across files.
	Groups []string

	// MD5 is the hex-encoded MD5 digest of article IDs. Used for
	// duplicate-job detection against history (Tranche B / Step 1.3).
	MD5 string

	// AvgAge is the mean posting date across the job's files, used
	// to sort the queue by "oldest first" and to trigger propagation
	// delay (downloads held back until articles have had time to
	// propagate across Usenet peers).
	AvgAge time.Time

	// Warning holds a human-readable warning message (e.g. "Duplicate NZB").
	// Usually accompanied by StatusPaused.
	Warning string

	// PostProc is set to true when the job is handed off to the
	// post-processor to prevent double-enqueuing.
	PostProc bool

	// residencyMu guards only the manifest/progress *pointer fields* below —
	// not their contents. Queue.Get/List hand out *Job pointers that alias
	// queue storage, and lazy eviction (evictJobLocked) and lazy
	// hydration/promotion reassign these pointers under q.mu without the
	// caller's involvement. A caller holding an aliased pointer but not
	// q.mu — the documented, intended way to use Manifest()/Progress() —
	// would otherwise race those reassignments (issue #263).
	//
	// It is a sync.RWMutex value, not a *sync.RWMutex, so every
	// construction path (struct literals, a zero-value `var job Job`,
	// json.Unmarshal) gets a ready-to-use mutex for free with no
	// initializer to forget. A prior attempt at this fix used a lazily
	// initialized *sync.RWMutex ("if j.mu == nil { j.mu = new(...) }"),
	// which is itself an unsynchronized data race on first concurrent use
	// and was rejected — see PR history for #263.
	//
	// This does NOT protect JobProgress's fields, which are mutated in
	// place under q.mu by design (see the package doc comment); a reader
	// holding only residencyMu can still observe a torn read of, e.g.,
	// job.progress.done mid-update. Only the pointer swap itself — "which
	// Manifest/JobProgress does this Job currently point to" — is
	// synchronized.
	residencyMu sync.RWMutex
	manifest    *Manifest
	progress    *JobProgress

	// Manifest-derived scalars, set from the manifest at Add and again
	// anywhere else a manifest is attached to or rebuilt on this Job (see
	// setScalarsFromManifest), so that reporting paths never need a
	// resident manifest — see docs/queue-lifecycle.md. For the overwhelming
	// majority of a job's life these values never change, since the
	// manifest itself is immutable after parse; DiscardDeferredPar2 is the
	// one exception, rebuilding the manifest to drop discarded recovery
	// volumes, and re-syncs these fields when it does.
	//
	// Guarded by residencyMu, not by q.mu. The getters below are exported
	// and documented as never needing a resident manifest, which invites
	// the same lock-free external use Manifest()/Progress() document as
	// safe — and an external caller cannot take q.mu. Writes go through
	// setScalarsFromManifest/setAggregateScalarsFromFiles, which take the
	// write lock; both are always called with q.mu held and never with
	// residencyMu already held, so the q.mu -> residencyMu order matches
	// setResidency's and cannot self-deadlock.
	totalBytes  int64
	numFiles    int
	numArticles int
	par2Bytes   int64
	par2Files   int

	// manifestRowsStale records that this job's persisted job_files rows no
	// longer describe its in-memory manifest, so nothing may write them by
	// file_index until a wholesale rewrite reconciles the two.
	//
	// One thing sets it: a DiscardDeferredPar2 whose Store.ReplaceManifest
	// failed. The discard shrinks the file set in memory and is deliberately
	// not rolled back, so the rows keep the pre-discard shape while the
	// manifest has the new one — and dropping a non-final file renumbers
	// every index after it, so row N now belongs to a different file than
	// manifest entry N.
	//
	// SQLiteStore.updateTx writes job_files with UPDATE ... WHERE
	// file_index = ?, taking each value from the live manifest and never
	// touching the identity columns. Under that disagreement it splices one
	// file's progress onto its pre-discard neighbour's row, silently, on
	// every checkpoint tick until the process restarts (#310).
	//
	// Not persisted. That is a limitation, not a safety property: it holds
	// only when the failure left both on-disk artifacts at the pre-discard
	// shape, where they still agree with each other. ReplaceManifest writes
	// the manifest blob before opening the transaction that rewrites
	// job_files, and a crash in that window leaves the new blob beside the
	// old rows with no in-memory flag to survive it. SQLiteStore.Get then
	// sizes progress from the new manifest and fills it by file_index from
	// the old rows, with no describesSameJobAs check — that guard runs on
	// hydrateSnapshot and hydrateJobLocked, not the boot path — so the
	// splice becomes permanent and undetected. The window predates this
	// flag and is not closed by it; see #310's discussion and #278.
	//
	// Guarded by residencyMu, alongside fileSetGen.
	manifestRowsStale bool

	// fileSetGen counts the times this job's file set has been rebuilt.
	// Only DiscardDeferredPar2 bumps it — nothing else changes which files a
	// job has after Add.
	//
	// It exists so a rewrite that completed outside the queue lock can tell
	// whether it still speaks for the job. Pointer identity on the manifest
	// cannot: eviction sets the pointer to nil and rehydration installs a
	// freshly deserialized *Manifest, so a job that merely churned through
	// the active set would refuse every subsequent clear and leave
	// manifestRowsStale raised for the rest of the process — freezing its
	// per-file persistence. Eviction does not change a job's file set, so it
	// must not invalidate a rewrite; a second discard does, and does bump
	// this.
	//
	// Guarded by residencyMu.
	fileSetGen uint64

	// hydrateErr records why an attempt to load this job's manifest from
	// disk failed, and is nil when no attempt failed. Guarded by
	// residencyMu alongside the pointers it explains.
	//
	// Without it, "manifest is nil" has two meanings a consumer cannot tell
	// apart: the job was evicted, which is routine, or its manifest file is
	// unreadable, which is data loss. hydrateSnapshot used to leave the
	// second looking exactly like the first — silently — so the paths that
	// dereference a manifest degraded or panicked on corruption without
	// anyone learning it had happened.
	hydrateErr error
}

// setScalarsFromManifest copies m's five totals onto j. Centralizing this
// assignment matters: a review of this feature (Task 3) caught two sites —
// hydrateSnapshot and PromoteNext — that attached a manifest to a Job
// without it, leaving totalBytes/numFiles/numArticles/par2Bytes/par2Files at
// zero despite a manifest being in hand. Every site that assigns j.manifest
// (directly or via setResidency) or rebuilds it in place must call this too.
func (j *Job) setScalarsFromManifest(m *Manifest) {
	// Read m's totals before taking the lock: m is immutable after parse,
	// and this keeps the critical section to the five assignments.
	totalBytes, numFiles := m.TotalBytes(), m.NumFiles()
	numArticles, par2Bytes, par2Files := m.NumArticles(), m.Par2Bytes(), m.Par2Files()

	j.residencyMu.Lock()
	defer j.residencyMu.Unlock()
	j.totalBytes = totalBytes
	j.numFiles = numFiles
	j.numArticles = numArticles
	j.par2Bytes = par2Bytes
	j.par2Files = par2Files
}

// setAggregateScalarsFromFiles sets totalBytes/numFiles/numArticles from
// values aggregated over job_files (SUM(bytes), COUNT(*), SUM(article_count))
// rather than from a resident Manifest. Used by SQLiteStore.Get/List for a
// job that is non-resident and has never been promoted/snapshotted
// in-process — the case Task 3 left as a documented gap, where these
// scalars would otherwise silently read as zero.
//
// par2Bytes/par2Files are deliberately left untouched: job_files'
// is_par2_recovery flags only recovery volumes, while the manifest's
// Par2Bytes/Par2Files also count the par2 index file, so the two are not
// equivalent and reconstructing the par2 pair from is_par2_recovery would
// silently produce an undercount rather than the value the manifest would
// have produced. They come from the jobs row instead — see
// setPar2ScalarsFromStore.
func (j *Job) setAggregateScalarsFromFiles(totalBytes int64, numFiles, numArticles int) {
	j.residencyMu.Lock()
	defer j.residencyMu.Unlock()
	j.totalBytes = totalBytes
	j.numFiles = numFiles
	j.numArticles = numArticles
}

// setPar2ScalarsFromStore sets par2Bytes/par2Files from the jobs row, for a
// job SQLiteStore.Get loaded without a resident manifest. It is the par2
// counterpart to setAggregateScalarsFromFiles: those three scalars can be
// aggregated out of job_files, these two cannot (see that method), so they
// get columns of their own.
func (j *Job) setPar2ScalarsFromStore(par2Bytes int64, par2Files int) {
	j.residencyMu.Lock()
	defer j.residencyMu.Unlock()
	j.par2Bytes = par2Bytes
	j.par2Files = par2Files
}

// TotalBytes returns the job's total size in bytes. Total: never requires a
// resident manifest. Safe to call without the queue lock — like
// Manifest()/Progress(), it takes the job's own residency lock, so a
// promotion or a DiscardDeferredPar2 manifest rebuild running concurrently
// under q.mu cannot race it.
func (j *Job) TotalBytes() int64 {
	j.residencyMu.RLock()
	defer j.residencyMu.RUnlock()
	return j.totalBytes
}

// NumFiles returns the job's file count. Total; lock-free-safe, see TotalBytes.
func (j *Job) NumFiles() int {
	j.residencyMu.RLock()
	defer j.residencyMu.RUnlock()
	return j.numFiles
}

// NumArticles returns the job's article count. Total; lock-free-safe, see TotalBytes.
func (j *Job) NumArticles() int {
	j.residencyMu.RLock()
	defer j.residencyMu.RUnlock()
	return j.numArticles
}

// Par2Bytes returns the total size of the job's par2 files. Total;
// lock-free-safe, see TotalBytes.
func (j *Job) Par2Bytes() int64 {
	j.residencyMu.RLock()
	defer j.residencyMu.RUnlock()
	return j.par2Bytes
}

// Par2Files returns the job's par2 file count. Total; lock-free-safe, see TotalBytes.
func (j *Job) Par2Files() int {
	j.residencyMu.RLock()
	defer j.residencyMu.RUnlock()
	return j.par2Files
}

// Manifest returns the job's immutable article/file structure, or an error
// explaining why it is unavailable. Safe to call without the queue lock: it
// takes the job's own residency lock to synchronize against concurrent
// eviction/hydration/promotion, which reassign this pointer under q.mu. The
// returned Manifest's contents are immutable after construction, so nothing
// further needs guarding once the pointer itself is safely read.
//
// It returns an error rather than a nil pointer so that depending on an
// evictable manifest is a compile error until the absent case is handled.
// Residency is a value the caller cannot see and does not control, and a
// silent nil produced eight defects across #258 and #260-#265 — including
// one whose dereference was a call away from the site that looked wrong.
//
// The two failures are distinct and callers usually want to treat them
// differently:
//
//   - ErrJobNotResident — the manifest was evicted. Ordinary: every queued
//     and paused job is in this state once the active set is full.
//   - anything else — the manifest could not be loaded. That is data loss,
//     recorded by both hydration paths (hydrateSnapshot for snapshot clones,
//     hydrateJobLocked for the live queue entry), and should not be handled
//     as routine absence.
//
// Use errors.Is(err, ErrJobNotResident) to tell them apart. The five
// promoted scalars (TotalBytes, NumFiles, NumArticles, Par2Bytes,
// Par2Files) and Progress() never fail, so reporting paths should use those
// rather than reaching for a manifest they do not need.
func (j *Job) Manifest() (*Manifest, error) {
	j.residencyMu.RLock()
	defer j.residencyMu.RUnlock()
	if j.manifest != nil {
		return j.manifest, nil
	}
	if j.hydrateErr != nil {
		return nil, j.hydrateErr
	}
	return nil, fmt.Errorf("%w: %s", ErrJobNotResident, j.ID)
}

// Progress returns the job's mutable per-article/per-file state. Safe to
// call without the queue lock for the same reason as Manifest: the
// residency lock only guards which *JobProgress this Job currently points
// to, not the JobProgress's contents. Those fields are mutated in place by
// Queue methods holding q.mu (see the package doc comment) and are NOT
// synchronized by this call — a caller reading through the returned
// pointer concurrently with such a mutation still observes a data race on
// the individual fields. JobProgress exposes no mutating exported method,
// so external packages cannot introduce new mutation races of their own,
// but they can still race the package's internal ones by reading at the
// wrong time.
func (j *Job) Progress() *JobProgress {
	j.residencyMu.RLock()
	defer j.residencyMu.RUnlock()
	return j.progress
}

// ManifestRowsStale reports whether this job's persisted job_files rows are
// known not to describe its current manifest. See the field's doc comment.
//
// Exported so the store can consult a snapshot it was handed. A false here is
// not proof the two agree — nothing verifies that — only that no failure this
// process saw made them disagree.
func (j *Job) ManifestRowsStale() bool {
	j.residencyMu.RLock()
	defer j.residencyMu.RUnlock()
	return j.manifestRowsStale
}

// setManifestRowsStale records or clears the disagreement. Clearing it is a
// claim that every job_files row for this job has just been rewritten from
// the current manifest, which only Store.ReplaceManifest does.
func (j *Job) setManifestRowsStale(stale bool) {
	j.residencyMu.Lock()
	defer j.residencyMu.Unlock()
	j.manifestRowsStale = stale
}

// FileSetGen returns how many times this job's file set has been rebuilt.
// Paired with clearManifestRowsStaleIfGen to make a rewrite that ran outside
// the queue lock verifiable after the fact.
func (j *Job) FileSetGen() uint64 {
	j.residencyMu.RLock()
	defer j.residencyMu.RUnlock()
	return j.fileSetGen
}

// bumpFileSetGen records that j's file set has been rebuilt, invalidating any
// in-flight rewrite of the previous one.
func (j *Job) bumpFileSetGen() {
	j.residencyMu.Lock()
	defer j.residencyMu.Unlock()
	j.fileSetGen++
}

// clearManifestRowsStaleIfGen clears the flag only while j's file set is
// still the one whose rows were written — gen is the generation the writer
// observed before starting.
//
// The check is what makes clearing safe from outside the queue lock. A
// discard that rebuilt the file set since then raises the flag for its own
// attempt and owns it from that point; a rewrite of the file set it replaced
// must not answer for it.
//
// Deliberately not keyed on the manifest pointer, which was this check's
// first form and was wrong: eviction nils the pointer and rehydration
// installs a new one, so ordinary residency churn between the write and the
// clear would refuse it forever. Eviction does not change a job's files.
func (j *Job) clearManifestRowsStaleIfGen(gen uint64) bool {
	j.residencyMu.Lock()
	defer j.residencyMu.Unlock()
	if j.fileSetGen != gen {
		return false
	}
	j.manifestRowsStale = false
	return true
}

// setResidency atomically swaps the manifest/progress pointer pair. Queue
// package code that reassigns these fields on a Job already aliased into
// q.jobs/q.byID (eviction, promotion, hydration, retry-discard) must go
// through this rather than raw field assignment, so a concurrent
// Manifest()/Progress() caller outside q.mu never observes one field
// updated and the other stale.
//
// Unexported deliberately: it is an internal pointer-swap primitive for
// code that already holds q.mu, not a public mutation API. Adding an
// exported SetManifest/SetProgress would contradict this package's doc
// comment, which requires all Progress mutation to go through Queue
// methods holding the write lock.
//
// Not used by NewJob or UnmarshalJSON: those construct a Job that is not
// yet reachable by any other goroutine, so there is nothing to
// synchronize against.
func (j *Job) setResidency(m *Manifest, p *JobProgress) {
	j.residencyMu.Lock()
	j.manifest = m
	j.progress = p
	if m != nil {
		j.hydrateErr = nil
	}
	j.residencyMu.Unlock()
}

// setHydrateFailure is setResidency's counterpart for a hydration attempt
// that failed: it clears the manifest, installs p as the job's progress, and
// records err as the reason, all under one write lock. The three go together
// for the same reason setResidency's pair does — a concurrent Manifest()
// caller must not observe the cleared manifest before the reason that
// explains it, or it would report routine eviction for data loss.
//
// p is the progress to leave behind: progress is always resident
// (docs/queue-lifecycle.md), so a failed hydration restores whatever was
// accurate before the attempt rather than the all-zero JobProgress that was
// built for the store to fill in.
//
// The one caller permitted to pass nil is SQLiteStore.Get, on the load path,
// where there is no "before the attempt" to restore — the job is being built.
// nil is the signal Queue.Load reads as "size this from job_files", and Load
// installs the result before the job reaches q.byID, so no other goroutine can
// observe the gap. Anywhere else nil would leave a live job without progress
// and break the always-resident guarantee outright.
//
// The error is not cleared here; a later successful hydration clears it via
// setResidency, because succeeding means the earlier failure no longer
// describes the job.
func (j *Job) setHydrateFailure(p *JobProgress, err error) {
	j.residencyMu.Lock()
	defer j.residencyMu.Unlock()
	j.manifest = nil
	j.progress = p
	j.hydrateErr = err
}

// JobPhase represents the high-level operational phase of a download job.
type JobPhase int

// JobPhase enum values.
const (
	PhasePending JobPhase = iota
	PhaseActive
	PhaseProcessing
	PhasePaused
	PhaseTerminal
)

// Phase returns the JobPhase corresponding to the job's current Status.
//
// The terminal arm is listed explicitly rather than left to the default.
// Without it PhaseTerminal was unreachable — declared, given a String() arm
// and written into docs/queue-lifecycle.md's contract, while every
// completed, failed and deleted job reported PhasePending. Both phases are
// non-resident, so nothing observably misbehaved, which is why it went
// unnoticed; but a parked failure and a job awaiting dispatch are different
// states and anything branching on the phase would have conflated them.
//
// StatusGrabbing and StatusChecking fall to PhasePending. They are
// SABnzbd-compatibility vocabulary that the API reports (see
// stageFromStatus) and that no code path currently assigns to a job. Note
// that this is a fact about the callers, not an invariant the type enforces:
// status.go's transition table constrains SetStatus/SetStatusIf, but the load
// paths bypass it — SQLiteStore.Get and Job.UnmarshalJSON both assign Status
// straight from a persisted string without validating it. Nothing writes
// those two strings today, so the pending mapping is never exercised; it is
// the safe answer if that ever changes.
func (j *Job) Phase() JobPhase {
	switch j.Status {
	case constants.StatusQueued, constants.StatusPropagating:
		return PhasePending
	case constants.StatusDownloading, constants.StatusFetching:
		return PhaseActive
	case constants.StatusVerifying, constants.StatusQuickCheck, constants.StatusRepairing,
		constants.StatusExtracting, constants.StatusMoving, constants.StatusRunning:
		return PhaseProcessing
	case constants.StatusPaused:
		return PhasePaused
	case constants.StatusCompleted, constants.StatusFailed, constants.StatusDeleted:
		return PhaseTerminal
	default:
		return PhasePending
	}
}

// IsResident returns true if the phase requires an in-memory resident manifest and progress.
func (p JobPhase) IsResident() bool {
	return p == PhaseActive || p == PhaseProcessing
}

func (p JobPhase) String() string {
	switch p {
	case PhasePending:
		return "Pending"
	case PhaseActive:
		return "Active"
	case PhaseProcessing:
		return "Processing"
	case PhasePaused:
		return "Paused"
	case PhaseTerminal:
		return "Terminal"
	default:
		return "Unknown"
	}
}

const (
	// earlyAbortSample is the number of articles that must resolve
	// before the early-abort heuristic fires. Too small → false
	// positives on slow starts; too large → wasted bandwidth.
	earlyAbortSample = 10

	// earlyAbortThreshold is the failure rate (0.0–1.0) above which
	// the job is considered DMCA'd or expired. 80% matches SABnzbd's
	// heuristic for fully-removed NZBs.
	earlyAbortThreshold = 0.80
)

// IsEarlyAbort returns true if the job should be aborted based on the
// first-article failure rate. It checks after earlyAbortSample articles
// have resolved: if 80%+ failed, the download is likely DMCA'd or
// expired and further downloading wastes bandwidth.
//
// Must be called under the queue's write lock.
func (j *Job) IsEarlyAbort() bool {
	return j.progress.isEarlyAbort()
}

// JobFile is the intermediate, NZB-parsed shape NewJob builds before
// converting into a Manifest/JobProgress pair. It is not part of Job's
// runtime state — construction scaffolding only.
type JobFile struct {
	Subject        string
	Date           time.Time
	Articles       []JobArticle
	Bytes          int64
	IsPar2Recovery bool
	// Deferred marks a file whose articles are intentionally held back from
	// dispatch (on-demand par2: recovery volumes not downloaded until repair
	// is shown to be needed).
	Deferred bool
}

// JobArticle is the intermediate, NZB-parsed shape of a single article,
// consumed by newManifest during construction.
type JobArticle struct {
	ID     string
	Bytes  int
	Number int
}

// AddOptions carries the call-site arguments for NewJob. Zero values
// are valid and produce sensible defaults.
type AddOptions struct {
	// Filename is the original NZB filename (may include a path).
	Filename string

	// Name overrides the display name. Empty means "derive from
	// Filename by stripping extensions".
	Name string

	Password string
	URL      string
	Category string

	// PP is the post-processing level. types.PPInherit (-1) means
	// "inherit from the job's category config".
	PP int

	// Script is the post-processing script name. Empty means
	// "inherit from the job's category config".
	Script string

	// Priority defaults to the category's priority when set to
	// constants.DefaultPriority. Zero means PriorityNormal.
	Priority constants.Priority

	// Categories is the full category config list, used to resolve
	// sentinel values for PP, Script, and Priority. May be nil in
	// tests; sentinels are left as-is when nil.
	Categories []config.CategoryConfig

	// Logger, if non-nil, receives structured log output for PP/script/
	// priority resolution decisions. Useful for diagnosing "why did this
	// job get PP=0?" questions.
	Logger *slog.Logger

	// OnDemandPar2, when true, defers par2 recovery volumes at add-time so
	// they are only downloaded if repair is later shown to be needed. The
	// par2 index file is always downloaded. Set from
	// config.DownloadConfig.OnDemandPar2 by the caller.
	OnDemandPar2 bool
}

// NewJob converts parser output plus caller options into a runtime
// Job ready to hand to Queue.Add. It allocates a fresh random ID and
// builds the job's Manifest/JobProgress from the parsed file/article
// structure.
//
// Returns an error only if the OS entropy source fails — treat that
// as fatal; the daemon has no safe fallback.
func NewJob(parsed *nzb.NZB, opts AddOptions, sOpts fsutil.SanitizeOptions) (*Job, error) {
	id, err := newJobID()
	if err != nil {
		return nil, err
	}

	name := opts.Name
	if name == "" {
		name = deriveName(opts.Filename)
	} else {
		// Strip .nzb extension when the caller supplies an explicit name
		// (e.g. Sonarr's nzbname parameter often includes it).
		name = stripNZBExt(name)
	}

	// 1. Apply regex-based cleanup (strip spam prefixes/suffixes)
	name = fsutil.CleanupName(name, sOpts)

	// 2. Apply filesystem sanitization rules.
	name = fsutil.SanitizeFolderName(name, sOpts)

	// Resolve sentinel values from the matching category config.
	// FindCategory never returns a zero CategoryConfig — when no
	// match is found it returns config.BuiltinDefaultCategory() with
	// PP=3, so the inherit path is always safe to take.
	pp := opts.PP
	script := opts.Script
	priority := opts.Priority
	// FindCategory never returns a zero CategoryConfig — when nothing
	// matches, it returns config.BuiltinDefaultCategory() (Name="Default",
	// PP=3). So the inherit path is always safe to take unconditionally
	// and cat.Name is always a useful value for the resolved-pp log line.
	ppReason := "explicit"
	cat := config.FindCategory(opts.Categories, opts.Category)
	if pp == types.PPInherit {
		pp = cat.PP
		ppReason = fmt.Sprintf("category %q", cat.Name)
	}
	if script == "" {
		script = cat.Script
	}
	// Clamp out-of-range PP to valid levels 0–3. SABnzbd's pp_to_opts
	// treats any value ≥ 3 as 3 (repair+unpack+delete); some configs
	// have legacy values like 7.
	if pp > types.PPDelete {
		pp = types.PPDelete
	} else if pp < types.PPNone {
		pp = types.PPNone
	}
	if priority == constants.DefaultPriority {
		p := cat.Priority
		if p < -128 || p > 127 {
			priority = constants.NormalPriority
		} else {
			priority = constants.Priority(p)
		}
	}

	if opts.Logger != nil {
		opts.Logger.Info("job PP resolved",
			"job_name", name,
			"category", opts.Category,
			"requested_pp", opts.PP,
			"resolved_pp", pp,
			"reason", ppReason,
			"script", script,
			"priority", priority.String(),
		)
	}

	status := constants.StatusQueued
	if priority == constants.PausedPriority {
		status = constants.StatusPaused
	}

	job := &Job{
		ID:       id,
		Filename: opts.Filename,
		Name:     name,
		Password: opts.Password,
		URL:      opts.URL,
		Category: opts.Category,
		Priority: priority,
		Status:   status,
		PP:       pp,
		Script:   script,
		Added:    time.Now().UTC(),
		Meta:     parsed.Meta,
		Groups:   parsed.Groups,
		MD5:      hex.EncodeToString(parsed.MD5[:]),
		AvgAge:   parsed.AvgAge,
	}

	files := make([]JobFile, 0, len(parsed.Files))
	for _, pf := range parsed.Files {
		isPar2 := isPar2File(pf.Subject)
		// A recovery volume (*.volNNN+MM.par2) carries redundancy; the par2
		// index file (no volume suffix) carries the per-file checksums we
		// need for verification and is therefore never deferred.
		isRecovery := isPar2 && isRecoveryVolume(pf.Subject)
		jf := JobFile{
			Subject:        pf.Subject,
			Date:           pf.Date,
			Bytes:          pf.Bytes,
			Articles:       make([]JobArticle, 0, len(pf.Articles)),
			IsPar2Recovery: isRecovery,
			// On-demand par2: hold recovery volumes back until repair is
			// shown to be needed. RemainingBytes still counts them (they are
			// part of the NZB) so the queue-progress denominator is unchanged;
			// they simply aren't dispatched while Deferred.
			Deferred: isRecovery && opts.OnDemandPar2,
		}
		for _, pa := range pf.Articles {
			jf.Articles = append(jf.Articles, JobArticle{
				ID:     pa.ID,
				Bytes:  pa.Bytes,
				Number: pa.Number,
			})
		}
		files = append(files, jf)
	}
	sortJobFiles(files)

	job.manifest = newManifest(files)
	job.progress = newJobProgress(job.manifest)
	job.setScalarsFromManifest(job.manifest)
	for fi, jf := range files {
		job.progress.files[fi].Deferred = jf.Deferred
	}
	return job, nil
}

// IsComplete returns true if all files in the job are marked complete.
// Deferred files (on-demand par2 recovery volumes held back from download)
// do not block completion — by design they are only fetched if repair is
// needed, so a job whose non-deferred files are all complete is "downloaded".
func (j *Job) IsComplete() bool {
	p := j.Progress()
	if p == nil || len(p.files) == 0 {
		return false
	}
	// Walk JobProgress's own file slice rather than the manifest's file
	// count. Progress is always resident, so completion is answerable for an
	// evicted job — this used to return false whenever the manifest was nil,
	// which is indistinguishable from a genuine "not complete" and made
	// startup finalization skip completed non-resident jobs outright.
	//
	// The slice rather than the promoted NumFiles scalar: the loop indexes
	// into p.files, so bounding it by p.files' own length keeps the count
	// and the data it indexes from ever disagreeing.
	//
	// An empty slice is not completion. A job whose progress carries no file
	// state — a row with no job_files, which Load sizes from an empty
	// count slice — would otherwise satisfy the loop vacuously and be
	// reported complete, and startup finalizes every job that reports
	// complete. Returning false leaves it visible in the queue instead of
	// silently swept into history. Before progress became the source of
	// truth this was masked for a non-resident job, which had a nil manifest
	// and so returned false for a different reason.
	for i := range len(p.files) {
		if p.FileDeferred(i) {
			continue
		}
		if !p.FileComplete(i) {
			return false
		}
	}
	return true
}

// HasDeferredPar2 reports whether the job currently has any deferred par2
// recovery volume. Safe to call on a snapshot (no lock needed).
func (j *Job) HasDeferredPar2() bool {
	if j.progress == nil {
		return false
	}
	return j.progress.HasDeferredPar2()
}

// DeferredRecoveryIndices returns the file indices of all currently-deferred
// par2 recovery volumes. Phase 1 un-defers this full set on damage; Phase 2
// selects a block-covering subset from it. Safe to call on a snapshot.
func (j *Job) DeferredRecoveryIndices() []int {
	if j.progress == nil {
		return nil
	}
	return j.progress.DeferredRecoveryIndices()
}

// ResetForRetry resets a completed/failed job back to a fresh downloadable
// state, preserving the Manifest but selectively resetting the existing
// Progress in place: RemainingBytes is re-added only for articles actually
// reset (not blanket-recomputed to TotalBytes), and a file's Complete flag
// is cleared only if at least one of its articles was reset. Can be called
// prior to re-adding or during Queue.Retry.
func (j *Job) ResetForRetry() {
	j.Status = constants.StatusQueued
	j.PostProc = false
	// This early return is the one silent skip #261 catalogued that is left
	// unconverted, because it is unreachable and the alternative costs more
	// than it buys. Both callers guarantee a hydrated job: Queue.Retry runs
	// hydrateJobLocked first and fails closed on error, and the history-retry
	// path (app.rebuildJobFromNZB) builds the job with NewJob, which assigns
	// manifest and progress together on its only success path — see the
	// newManifest/newJobProgress pair in NewJob. It used to build it with
	// queue.LoadJob, removed in #298; the guarantee survived the change of
	// constructor. Giving this an error return to report a state
	// neither caller can produce would change an exported signature for a
	// branch that cannot execute. If a third caller ever arrives, it must
	// hydrate first — the reset is defined in terms of which articles failed,
	// and that is only knowable against a live manifest.
	if j.progress == nil || j.manifest == nil {
		return
	}
	j.progress.downloadStarted = time.Time{}
	j.progress.downloadFinished = time.Time{}
	j.progress.serverStats = nil
	j.progress.failedBytes = 0
	j.progress.earlyAborted = false
	j.progress.par2Recovered = false
	j.progress.par2ReleaseReason = ""

	m := j.manifest
	for fi := range m.NumFiles() {
		anyReset := false
		lo, hi := m.FileRange(fi)
		for i := lo; i < hi; i++ {
			if !j.progress.failed.Get(i) {
				continue
			}
			j.progress.done.Clear(i)
			j.progress.failed.Clear(i)
			j.progress.remainingBytes += int64(m.ArticleBytes(i))
			anyReset = true
		}
		if anyReset {
			j.progress.files[fi].Complete = false
		}
	}
	j.progress.recompute(j.manifest)
}

// MarkDownloadFinished sets the job's download-finished timestamp. Intended
// for callers that already hold an owned clone (e.g. a Queue.SnapshotJob
// result) rather than a live queue reference — it performs no queue
// locking of its own.
func (j *Job) MarkDownloadFinished(t time.Time) {
	if j.progress != nil {
		j.progress.downloadFinished = t
	}
}

// jobJSON is Job's on-disk shape: header fields plus the two nested
// documents. Field names here are free to read well: this on-disk format
// has no wire-compatibility requirement to preserve — the only external
// compatibility surface is the SABnzbd-compatible HTTP API, which builds
// its own response DTOs from accessor calls rather than marshaling Job
// directly.
type jobJSON struct {
	ID        string              `json:"id"`
	Filename  string              `json:"filename"`
	NZBBackup string              `json:"nzb_backup,omitempty"`
	Name      string              `json:"name"`
	Password  string              `json:"password,omitempty"` //nolint:gosec // G117: NZB archive password, not a credential
	URL       string              `json:"url,omitempty"`
	Category  string              `json:"category,omitempty"`
	Priority  constants.Priority  `json:"priority"`
	Status    constants.Status    `json:"status"`
	PP        int                 `json:"pp"`
	Script    string              `json:"script,omitempty"`
	Added     time.Time           `json:"added"`
	MD5       string              `json:"md5"`
	AvgAge    time.Time           `json:"avg_age"`
	Groups    []string            `json:"groups,omitempty"`
	Meta      map[string][]string `json:"meta,omitempty"`
	Warning   string              `json:"warning,omitempty"`
	PostProc  bool                `json:"post_proc,omitempty"`

	Manifest *Manifest    `json:"manifest"`
	Progress *JobProgress `json:"progress"`
}

// MarshalJSON implements json.Marshaler.
func (j *Job) MarshalJSON() ([]byte, error) {
	return json.Marshal(jobJSON{ //nolint:gosec // G117: NZB archive password, not a credential
		ID:        j.ID,
		Filename:  j.Filename,
		NZBBackup: j.NZBBackup,
		Name:      j.Name,
		Password:  j.Password,
		URL:       j.URL,
		Category:  j.Category,
		Priority:  j.Priority,
		Status:    j.Status,
		PP:        j.PP,
		Script:    j.Script,
		Added:     j.Added,
		MD5:       j.MD5,
		AvgAge:    j.AvgAge,
		Groups:    j.Groups,
		Meta:      j.Meta,
		Warning:   j.Warning,
		PostProc:  j.PostProc,
		Manifest:  j.manifest,
		Progress:  j.progress,
	})
}

// UnmarshalJSON implements json.Unmarshaler. It does not recompute
// transient counters (PendingArticles/ArticlesResolved/ArticlesFailed); a
// caller must invoke the JobProgress-equivalent recompute afterward.
//
// There is no production caller. Job's JSON form existed for the history
// retry payload, whose reader (queue.LoadJob) went in #298; the only thing
// exercising it now is TestPersistenceRoundTrip_AccessorParity. Tracked as
// dead code in #304 rather than removed here, because the honest fix is to
// move that test's assertions onto the store rather than delete them.
func (j *Job) UnmarshalJSON(data []byte) error {
	var jj jobJSON
	if err := json.Unmarshal(data, &jj); err != nil {
		return err
	}
	j.ID = jj.ID
	j.Filename = jj.Filename
	j.NZBBackup = jj.NZBBackup
	j.Name = jj.Name
	j.Password = jj.Password
	j.URL = jj.URL
	j.Category = jj.Category
	j.Priority = jj.Priority
	j.Status = jj.Status
	j.PP = jj.PP
	j.Script = jj.Script
	j.Added = jj.Added
	j.MD5 = jj.MD5
	j.AvgAge = jj.AvgAge
	j.Groups = jj.Groups
	j.Meta = jj.Meta
	j.Warning = jj.Warning
	j.PostProc = jj.PostProc
	j.manifest = jj.Manifest
	j.progress = jj.Progress
	return nil
}

// deriveName strips directory components and the extension from path.
// For "/watch/My.Show.S01E02.nzb" returns "My.Show.S01E02". A ".nzb.gz"
// or ".nzb.bz2" double extension is collapsed to the bare stem too.
// stripNZBExt removes .nzb, .nzb.gz, and .nzb.bz2 extensions from name.
// Used for both explicit names and derived names to prevent download
// directories like "movie.nzb/".
func stripNZBExt(name string) string {
	lower := strings.ToLower(name)
	for _, suffix := range []string{".nzb.gz", ".nzb.bz2", ".nzb"} {
		if strings.HasSuffix(lower, suffix) {
			return name[:len(name)-len(suffix)]
		}
	}
	return name
}

func deriveName(path string) string {
	base := filepath.Base(path)
	if stripped := stripNZBExt(base); stripped != base {
		return stripped
	}
	if ext := filepath.Ext(base); ext != "" {
		return base[:len(base)-len(ext)]
	}
	return base
}

// newJobID returns a 16-character lowercase hex string backed by 8
// bytes (64 bits) of OS entropy. Collisions are vanishingly unlikely
// within any realistic job population; the queue still rejects Add
// when an ID collides.
func newJobID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
