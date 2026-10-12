package job

import (
	"errors"
	"fmt"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
)

// ErrNotResident is returned by Manifest when the job's content tier is not
// loaded. It is distinct from a hydration failure on purpose: "evicted" is
// routine and "unreadable on disk" is data loss, and internal/queue's
// hydrateErr existed because those two had been indistinguishable.
var ErrNotResident = errors.New("job: content tier not resident")

// AttachContent installs the manifest and derives the progress record from it.
//
// It is the sole path that DERIVES a fresh progress record from a manifest:
// progress is computed here from m and never supplied by an AttachContent
// caller, which is what makes "progress describes this manifest" hold for a
// job starting out rather than being merely asserted.
//
// It is the only writer of the progress pointer, which
// TestProgressPointerWriters_MatchTheEnumerationStatedInProse pins.
// RestoreContent, below, installs a manifest beside the progress record the
// job already holds and never replaces that record.
func (j *Job) AttachContent(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("job %s: AttachContent: nil manifest", j.id)
	}
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	// Refuse a second attach rather than silently replacing a live record.
	// newJobProgress below builds a FRESH JobProgress, so a re-attach would
	// discard every done/failed bit and both stamps with no error — and since
	// the first attach zeroed the Job-level copy, there would be nothing left
	// to seed the replacement from either. Callers gate on this today
	// (residency.Hydrate takes the RestoreContent arm when HasProgress() is
	// true, and the ingest path builds a fresh Job), which is what makes
	// this an assertion of an invariant rather than a behaviour change.
	if j.progress != nil || j.manifest != nil {
		return fmt.Errorf("job %s: AttachContent: content already attached; use RestoreContent to install a manifest alongside existing progress", j.id)
	}
	j.manifest = m
	j.progress = newJobProgress(m)
	j.totalBytes = m.TotalBytes()
	j.recoveryBytes = m.RecoveryBytes()
	j.recoveryFiles = m.RecoveryFiles()
	// Seed from state restored before this job had a JobProgress, then zero it
	// so the Job-level copy and the progress record are never both live. These
	// go through the existing restore doors rather than assigning the fields:
	// restoreDownloadStamps owns the isJobStamp filter, and a second
	// implementation of it here is exactly the divergence that filter exists
	// to prevent. Direct field access, not the exported setters — contentMu is
	// held and sync.RWMutex is not reentrant.
	j.progress.restoreDownloadStamps(j.restoredDLStarted, j.restoredDLFinished)
	j.progress.restorePar2ReleaseReason(j.restoredPar2Reason)
	j.progress.restorePar2Recovered(j.restoredPar2Recovered)
	j.restoredPar2Reason = ""
	j.restoredDLStarted = time.Time{}
	j.restoredDLFinished = time.Time{}
	j.restoredPar2Recovered = false
	return nil
}

// RestoreContent re-attaches a manifest to the job's own progress record. It
// is AttachContent's counterpart for a job whose manifest was evicted, and it
// installs the manifest only: the progress record stays the one the job
// already holds.
//
// The record is recomputed in place, under the same lock its writers take, and
// never replaced. Writes that need no manifest — MarkArticleFailed and
// ClearArticleEmitted while evicted, and the progress-tier setters — land on
// that record at any time, so a copy taken earlier and installed here would
// silently discard whatever arrived in between. recompute is what folds the
// bits those writes left into the counters.
//
// It verifies the two describe the same job rather than trusting the caller:
// describesSameJobAs compares article and file counts, and a mismatch here
// means the manifest read from disk belongs to a different revision.
func (j *Job) RestoreContent(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("job %s: RestoreContent: nil manifest", j.id)
	}
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: RestoreContent: no progress record to restore onto; use AttachContent", j.id)
	}
	if !j.progress.describesSameJobAs(m) {
		return fmt.Errorf("job %s: RestoreContent: progress describes a different manifest", j.id)
	}
	j.progress.recompute(m)
	j.manifest = m
	j.totalBytes = m.TotalBytes()
	j.recoveryBytes = m.RecoveryBytes()
	j.recoveryFiles = m.RecoveryFiles()
	return nil
}

// Evict drops the manifest and keeps the progress record.
//
// Progress is always resident by design (docs/job-lifecycle.md's three
// tiers): it is small, and the abort checks and the queue listing read it for
// jobs that are not running. Only the manifest, which is sized by article
// count, is evictable.
func (j *Job) Evict() {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.manifest = nil
}

// Resident reports whether the manifest is loaded.
func (j *Job) Resident() bool {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.manifest != nil
}

// Manifest returns the resident manifest, or ErrNotResident.
func (j *Job) Manifest() (*Manifest, error) {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.manifest == nil {
		return nil, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	return j.manifest, nil
}

// Progress returns a point-in-time clone of the always-resident progress record,
// or nil before any content has been attached.
func (j *Job) Progress() *JobProgress {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return nil
	}
	return j.progress.clone()
}

// HasProgress reports whether the progress tier is resident.
func (j *Job) HasProgress() bool {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.progress != nil
}

// ProgressFigures returns (expected, remaining, failed) bytes under a single
// contentMu.RLock() acquisition, or (0, 0, 0) if progress is not resident.
//
// Callers computing differences (such as downloaded = expected - failed - remaining)
// or completion percentages should call this rather than separate point reads to
// prevent observing torn states across concurrent article updates or par2 releases.
func (j *Job) ProgressFigures() (expected, remaining, failed int64) {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return 0, 0, 0
	}
	return j.progress.ProgressFigures()
}

// ExpectedBytes returns what the job is expected to fetch, or 0 if progress is not resident.
func (j *Job) ExpectedBytes() int64 {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return 0
	}
	return j.progress.ExpectedBytes()
}

// RemainingBytes returns what is still to fetch, or 0 if progress is not resident.
func (j *Job) RemainingBytes() int64 {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return 0
	}
	return j.progress.RemainingBytes()
}

// FailedBytes returns the total failed article bytes, or 0 if progress is not resident.
func (j *Job) FailedBytes() int64 {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return 0
	}
	return j.progress.FailedBytes()
}

// ContentFailedBytes returns failed bytes excluding par2, or 0 if progress is not resident.
func (j *Job) ContentFailedBytes() int64 {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return 0
	}
	return j.progress.ContentFailedBytes()
}

// PendingArticles returns the number of un-downloaded articles, or 0 if progress is not resident.
func (j *Job) PendingArticles() int {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return 0
	}
	return j.progress.PendingArticles()
}

// DownloadStarted returns the time the first article began downloading, or
// zero if unstarted. Before the job has a JobProgress it reads the value
// restored from the store, so a restored job reports the same stamp whether or
// not it has been hydrated yet.
func (j *Job) DownloadStarted() time.Time {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return j.restoredDLStarted
	}
	return j.progress.DownloadStarted()
}

// DownloadBegun reports whether article bytes may exist under the job's name:
// at least one file has a resolved on-disk filename. The pipeline's
// registerFile sets it (RecordFileFilename) before it hands the file's first
// article to the assembler, which creates the file, on the success path and
// the permanent-failure path alike. Verification restores it
// (InstallFileVerification), and drops the rows of a complete=0 file recorded
// without one (verifyJobFiles), so a restored or retried job with done articles
// reports it too; ResetForRetry does not clear it. The writers of
// FileProgress.Filename are those two —
// `git grep -n '\]\.Filename = ' -- 'internal/job/*.go' ':!*_test.go'` returns 2 lines.
//
// Before the job has a JobProgress it reads the restored start stamp, which a
// job with a filename may lack; Dispatcher.SetName hydrates before Job.SetName
// asks.
//
// A job that has begun an attempt but fetched nothing (HasRun, which
// Queue.Advance sets for any ungated job on its first tick) does not report
// it, and neither does one with a failed article whose file was never
// registered.
func (j *Job) DownloadBegun() bool {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return !j.restoredDLStarted.IsZero()
	}
	for i := range j.progress.files {
		if j.progress.files[i].Filename != "" {
			return true
		}
	}
	return false
}

// DownloadFinished returns the time downloading completed, or zero if
// unfinished. Reads the restored value before hydration, as DownloadStarted does.
func (j *Job) DownloadFinished() time.Time {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return j.restoredDLFinished
	}
	return j.progress.DownloadFinished()
}

// Par2ReleaseReason returns the reason par2 articles were released, or empty
// if not released. Reads the restored value before hydration, as
// DownloadStarted does.
func (j *Job) Par2ReleaseReason() string {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return j.restoredPar2Reason
	}
	return j.progress.Par2ReleaseReason()
}

// SetFileFetchPolicy sets the fetch policy for file fi under the content lock.
func (j *Job) SetFileFetchPolicy(fi int, policy FetchPolicy) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fi < 0 || fi >= len(j.progress.files) {
		return fmt.Errorf("job %s: file index %d out of range", j.id, fi)
	}
	j.progress.setFileFetchPolicy(fi, policy)
	return nil
}

// setFileFetchPolicy is the one writer of a file's fetch policy:
// `git grep -nE '\.Fetch\s*=[^=]' -- '*.go' ':!*_test.go'` finds 1 line, the
// assignment below. SetFileFetchPolicy, Job.InstallFileVerification's
// restore, undeferRecovery and DiscardDeferredPar2 all call it. A fresh
// progress starts at the FetchAlways zero. The `job_files.fetch_policy` CHECK
// (0-2) is the only range guard a restored value has. The caller holds
// contentMu and has range-checked fi.
//
// It owns what the policy decides about the pending counters: a file counts
// pending work only while FetchAlways (countsPending), so it re-derives the
// file's Pending from its bits (pendingOf) in the same critical section. That
// needs no manifest, so it holds at any residency, and the four callers named
// above leave the counters to it (#903).
//
// It marks the file for Job.TakeFetchPolicyChanges whenever the stored value
// differs from the one the caller asked for or from the previous one, so the
// record learns of every change without its caller having to say so.
//
// A hold is not set over a release: once par2Recovered is set nothing
// releases a volume again, so FetchIfNeeded then becomes FetchAlways. A
// hydration restores a hold over a release in two ways — job_files and
// dispatch_jobs' par2_recovered are written by different writers, so a crash
// can land one release and not the other; and a content file's lost article
// releases the volumes installed before it, ahead of a later volume's
// restore.
func (p *JobProgress) setFileFetchPolicy(fi int, policy FetchPolicy) {
	stored := policy
	if stored == FetchIfNeeded && p.par2Recovered {
		stored = FetchAlways
	}
	f := &p.files[fi]
	if f.Fetch == stored && stored == policy {
		return
	}
	f.Fetch = stored
	p.setPending(fi, p.pendingOf(fi))
	if !f.fetchChanged {
		f.fetchChanged = true
		p.fetchChanges++
	}
}

// TakeFetchPolicyChanges returns the record state of every file whose fetch
// policy changed since the last take, and forgets the changes. The recorder
// takes them at each flush, so a fetch policy reaches job_files whichever
// path changed it. It returns nil when the job has no progress.
func (j *Job) TakeFetchPolicyChanges() []durability.FileState {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	p := j.progress
	if p == nil || p.fetchChanges == 0 {
		return nil
	}
	out := make([]durability.FileState, 0, p.fetchChanges)
	for fi := range p.files {
		f := &p.files[fi]
		if !f.fetchChanged {
			continue
		}
		f.fetchChanged = false
		out = append(out, p.fileState(fi))
	}
	p.fetchChanges = 0
	return out
}

// MarkArticleFailed records an article that will not be retried, charging
// par2 charge bytes for it, capped at the article's manifest size. WholeArticle
// charges that whole size. A smaller count is for an article only part of
// whose range is missing from the file, because other articles already wrote
// the rest (#432); zero charges nothing, and such a failure is resolved but
// not failed for the early-abort rate (#844). Any other negative charge is an
// error and changes nothing.
//
// It does not require the manifest. A fetch can complete after its job was
// evicted, so refusing an evicted job would lose the failure. Evicted, it
// records the bits and the charge alone: the counters and the early par2
// release need the manifest's file ranges and article sizes, and
// RestoreContent's recompute derives the counters from the bits when the job
// is next hydrated.
func (j *Job) MarkArticleFailed(artIdx int, charge int64) error {
	if charge < 0 && charge != WholeArticle {
		return fmt.Errorf("job %s: artIdx %d: negative charge %d", j.id, artIdx, charge)
	}
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	return j.markArticleFailed(artIdx, charge)
}

// markArticleFailed is the body of MarkArticleFailed, which
// Job.InstallFileVerification also calls. charge is the bytes the failure
// charges, or WholeArticle. The caller holds contentMu.
func (j *Job) markArticleFailed(artIdx int, charge int64) error {
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if artIdx < 0 || artIdx >= j.progress.TotalArticles() {
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
	if j.manifest == nil {
		if j.progress.setFailedBits(artIdx) {
			j.progress.limitCharge(artIdx, charge)
		}
		return nil
	}
	// A failure that charges nothing leaves par2 nothing to repair, so it is
	// no reason to fetch recovery volumes.
	if charged, ok := j.progress.markFailedUncovered(j.manifest, artIdx, charge); ok && charged > 0 {
		// Repairing never runs for a job whose Policy.Repair is false
		// (PP=0), so releasing its recovery volumes here would only spend
		// bandwidth nothing will use. This is the live-download half of the
		// gate; the Assessing-verdict half is app.maybeReleaseRecoveryVolumes.
		if j.policy.Repair && !j.progress.par2Recovered && j.manifest.RecoveryFiles() > 0 {
			if j.undeferRecovery(j.progress.DeferredRecoveryIndices()) {
				j.progress.setPar2ReleaseReason("permanent article download failure detected on active queue")
			}
		}
	}
	return nil
}

// MarkArticleEmitted records that an article has been handed to the downloader.
func (j *Job) MarkArticleEmitted(artIdx int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil || j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if artIdx < 0 || artIdx >= j.manifest.NumArticles() {
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
	j.progress.markEmitted(j.manifest, artIdx)
	return nil
}

// ClearArticleEmitted returns an article whose result will not be written to
// Outstanding.
//
// It does not require the manifest. Eviction keeps the emitted bit and
// ForEachUnfinishedArticle skips an article whose bit is set, so refusing an
// evicted job strands the article until a downloader reload or a restart.
// Evicted, it clears the bit alone, and RestoreContent's recompute restores the
// pending counts when the job is next hydrated.
func (j *Job) ClearArticleEmitted(artIdx int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if artIdx < 0 || artIdx >= j.progress.TotalArticles() {
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
	if j.manifest == nil {
		j.progress.emitted.Clear(artIdx)
		return nil
	}
	j.progress.clearEmitted(j.manifest, artIdx)
	return nil
}

// ForEachUnfinishedArticle invokes fn for each unfinished (not Done, not Emitted) article
// in FetchAlways files, under contentMu.RLock. Iteration stops when fn returns false.
func (j *Job) ForEachUnfinishedArticle(fn func(fileIdx int, artIdx int32, id string, bytes int, number int, subject string) bool) error {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.manifest == nil || j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	m := j.manifest
	p := j.progress
	if p.pendingArticles == 0 {
		return nil
	}
	for fi := range m.NumFiles() {
		if p.files[fi].Complete || p.files[fi].Pending == 0 || p.files[fi].Fetch != FetchAlways {
			continue
		}
		lo, hi := m.FileRange(fi)
		for i := lo; i < hi; i++ {
			if p.done.Get(i) || p.emitted.Get(i) {
				continue
			}
			//nolint:gosec // G115: article index is guaranteed non-negative and fits in int32
			if !fn(fi, int32(i), m.ArticleID(i), m.ArticleBytes(i), m.ArticleNumber(i), m.FileSubject(fi)) {
				return nil
			}
		}
	}
	return nil
}

// MarkFileComplete records that a file's articles have all been assembled.
//
// It writes the progress record alone, so it does not require the manifest:
// an evicted job takes the mark (#868). What must precede the mark — the CRC
// settle, the archive peek, the DirectUnpack feed — does need the manifest,
// and ordering the mark after it is the caller's.
func (j *Job) MarkFileComplete(fileIdx int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Complete = true
	return nil
}

// TotalBytes returns the total size of the job's files, preserved across eviction.
func (j *Job) TotalBytes() int64 {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.totalBytes
}

// RecoveryBytes returns the summed size of the job's par2 recovery volumes,
// preserved across eviction.
func (j *Job) RecoveryBytes() int64 {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.recoveryBytes
}

// SetRecoveryBytes records the summed size of the job's par2 recovery volumes.
//
// Writers of recoveryBytes — `git grep -n 'recoveryBytes\s*=' internal/job/`:
//   - AttachContent: derives truth from Manifest
//   - RestoreContent: derives truth from Manifest
//   - SetRecoveryBytes: restores persisted count from SQLite on startup prior to hydration
//
// Manifest hydration (AttachContent/RestoreContent) strictly supersedes any
// value restored from SQLite via SetRecoveryBytes.
func (j *Job) SetRecoveryBytes(n int64) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.recoveryBytes = n
}

// RecoveryFiles returns the job's par2 recovery volume count, preserved across eviction.
func (j *Job) RecoveryFiles() int {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.recoveryFiles
}

// RepairState reports whether this job's damaged content is within its par2
// recovery capacity.
func (j *Job) RepairState() RepairState {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return RepairUnknown
	}
	return RepairStateFrom(j.progress.ContentFailedBytes(), j.recoveryBytes, j.progress.HasPar2Files())
}

// NumFiles returns the number of files in the job if content has been attached,
// or 0 if neither progress nor manifest is resident.
func (j *Job) NumFiles() int {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress != nil {
		return len(j.progress.files)
	}
	if j.manifest != nil {
		return j.manifest.NumFiles()
	}
	return 0
}

// undeferRecovery releases each held volume of fileIdxs to FetchAlways and
// sets par2Recovered, reporting whether any was held. The caller holds
// contentMu and the job has progress.
func (j *Job) undeferRecovery(fileIdxs []int) bool {
	changed := false
	for _, fi := range fileIdxs {
		if fi < 0 || fi >= len(j.progress.files) || j.progress.files[fi].Fetch != FetchIfNeeded {
			continue
		}
		j.progress.setFileFetchPolicy(fi, FetchAlways)
		changed = true
	}
	if changed {
		j.progress.par2Recovered = true
	}
	return changed
}

// UndeferRecoveryVolumes marks the specified deferred recovery volumes as
// FetchAlways, re-activating them for download when repair is needed.
//
// Like DiscardDeferredPar2 it is a progress-tier write and takes effect at any
// residency: the policy, its pending counts and par2Recovered all live on the
// progress record, and an evicted job's articles are offered only after the
// hydration that re-attaches its manifest.
func (j *Job) UndeferRecoveryVolumes(fileIdxs []int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	for _, fi := range fileIdxs {
		if fi < 0 || fi >= len(j.progress.files) {
			return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fi)
		}
	}
	j.undeferRecovery(fileIdxs)
	return nil
}

// DeferredRecoveryIndices returns the indices of all files still held as FetchIfNeeded.
func (j *Job) DeferredRecoveryIndices() []int {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return nil
	}
	return j.progress.DeferredRecoveryIndices()
}

// HasDeferredPar2 reports whether any file is still held pending the CRC verdict.
func (j *Job) HasDeferredPar2() bool {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return false
	}
	return j.progress.HasDeferredPar2()
}

// UsesOnDemandPar2 reports whether any file is being withheld from download
// under a non-default fetch policy — either awaiting the CRC verdict
// (FetchIfNeeded) or already ruled unnecessary (FetchNever).
func (j *Job) UsesOnDemandPar2() bool {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return false
	}
	return j.progress.UsesOnDemandPar2()
}

// DiscardDeferredPar2 marks every recovery volume still awaiting the CRC
// verdict as never-fetch, reporting whether any file changed. Like the
// release, it takes effect at any residency.
func (j *Job) DiscardDeferredPar2() bool {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return false
	}
	changed := false
	for fi := range len(j.progress.files) {
		if j.progress.files[fi].Fetch == FetchIfNeeded {
			j.progress.setFileFetchPolicy(fi, FetchNever)
			changed = true
		}
	}
	return changed
}

// SetPar2ReleaseReason records the reason deferred recovery volumes were
// released or discarded.
//
// The else branch is not defensive padding: Par2ReleaseReason reads the
// restored field while progress is nil, so dropping the write there — as this
// did before #504 — would make a write-then-read on a job that has not been
// hydrated return the restored value instead of the one just written. Writing
// to whichever field is currently authoritative keeps the two in agreement.
func (j *Job) SetPar2ReleaseReason(reason string) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress != nil {
		j.progress.setPar2ReleaseReason(reason)
	} else {
		j.restoredPar2Reason = reason
	}
}

// CountUnfinishedArticles returns the count of articles not yet Done for the given file.
func (j *Job) CountUnfinishedArticles(fileIdx int) (int, error) {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.manifest == nil || j.progress == nil {
		return 0, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= j.manifest.NumFiles() {
		return 0, fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	var count int
	lo, hi := j.manifest.FileRange(fileIdx)
	for i := lo; i < hi; i++ {
		if !j.progress.done.Get(i) {
			count++
		}
	}
	return count, nil
}

// FileFetchPolicy reads one file's fetch policy under the content lock.
//
// It exists so a caller that wants a single policy does not have to go through
// Progress(), which returns a deep clone — every file, every bitset, every map
// — to answer a question about one int. seedJobFiles asks exactly that
// question, once per file.
//
// A non-resident job and an out-of-range index both read FetchAlways rather
// than erroring, matching JobProgress.FileFetchPolicy: the zero policy is the
// "fetch it" answer, so a caller that cannot learn otherwise downloads the
// file instead of silently skipping it.
func (j *Job) FileFetchPolicy(fi int) FetchPolicy {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.progress.FileFetchPolicy(fi)
}

// ErrNameChanged is RecordFileFilename's refusal of a filename resolved under a
// job name that SetName has since replaced.
var ErrNameChanged = errors.New("job: name changed since it was read")

// RecordFileFilename stores the resolved final filename on a file, provided
// the job is still named name: the name the caller resolved the file's
// directory from. Otherwise it records nothing and returns ErrNameChanged, and
// the caller resolves the file again under the current name.
//
// It holds j.mu across the name check and the write, and SetName holds it
// across its DownloadBegun check and its name write, so the two are ordered:
// either the filename is recorded first and SetName refuses with
// ErrDownloadBegun, or the rename lands first and this refuses.
//
// It writes the progress record alone, so it does not need the manifest and
// records at any residency: an eviction between the caller's resolution and
// this write must not lose the filename. It returns ErrNotResident only when
// the job has no progress record.
func (j *Job) RecordFileFilename(fileIdx int, name, filename string) error {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if j.name != name {
		return fmt.Errorf("job %s: file %d resolved under %q, now named %q: %w", j.id, fileIdx, name, j.name, ErrNameChanged)
	}
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= len(j.progress.files) {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Filename = filename
	return nil
}

// FileLocation returns the job's name and file fileIdx's recorded filename
// ("" when none is recorded, the job has no progress, or the index names no
// file), read together under j.mu and contentMu. SetName writes the name under
// j.mu, so no rename falls between the two reads.
func (j *Job) FileLocation(fileIdx int) (name, filename string) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.name, j.progress.FileFilename(fileIdx)
}

// ClearEmittedForReload resets Emitted and Failed flags for reload.
func (j *Job) ClearEmittedForReload(skipEmitted bool) (cleared, retained []int32) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return nil, nil
	}
	m := j.manifest
	for fi := range m.NumFiles() {
		c, r := j.progress.resetForReload(m, fi, !skipEmitted)
		cleared = append(cleared, c...)
		retained = append(retained, r...)
	}
	if len(cleared) > 0 || len(retained) > 0 {
		j.progress.recompute(m)
	}
	return cleared, retained
}

// ResetForRetry returns a failed job to a downloadable state, preserving
// what it already fetched.
//
// A file is left Complete only if every one of its articles is still done once
// the failed ones are reset. ForEachUnfinishedArticle skips a Complete file and
// IsComplete trusts the flag, and app.retryHistoryJob restores it from the
// retained history row: a file an earlier attempt finalized short arrives
// Complete while the articles that attempt could not fetch are not done.
//
// It clears the progress-tier par2 state but not the Job-level restored*
// fields, and the branch that makes that safe is that a retried job is always a
// fresh object: the only production caller is app.retryHistoryJob
// (`git grep -n '[j]\.ResetForRetry()' -- '*.go' ':!*_test.go'` returns 1 line),
// the body of RetryHistoryJob and of the finalizer's automatic retry, which
// rebuilds the job through BuildIngestJob's job.New + AttachContent rather than
// reusing the restored one, so restored* is already zero. A caller that
// retried a restored, never-hydrated job in place would return early here on
// the progress == nil guard and keep the old values.
func (j *Job) ResetForRetry() {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return
	}
	j.progress.earlyAborted = false
	j.progress.par2Recovered = false
	j.progress.clearPar2ReleaseReason()
	j.progress.clearDownloadStamps()

	// A bulk reset: clearDone maintains no counter, and the recompute below
	// derives every one of them from the bits.
	m := j.manifest
	var reset []int
	for fi := range m.NumFiles() {
		unresolved := false
		reset = reset[:0]
		lo, hi := m.FileRange(fi)
		for i := lo; i < hi; i++ {
			switch {
			case j.progress.failed.Get(i):
				reset = append(reset, i)
				unresolved = true
			case !j.progress.done.Get(i):
				unresolved = true
			}
		}
		j.progress.clearDone(fi, reset...)
		if unresolved {
			fp := &j.progress.files[fi]
			fp.Complete = false
			fp.AssembledCRC32 = 0
		}
	}
	j.progress.recompute(j.manifest)
}

// IsComplete reports whether all required files (FetchAlways) are complete.
func (j *Job) IsComplete() bool {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return false
	}
	for i := range j.progress.files {
		if j.progress.files[i].Fetch != FetchAlways {
			continue
		}
		if !j.progress.files[i].Complete {
			return false
		}
	}
	return true
}

// CheckEarlyAbort evaluates the early-abort heuristic and sets earlyAborted if triggered.
func (j *Job) CheckEarlyAbort() bool {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return false
	}
	return j.progress.isEarlyAbort()
}

// MarkJobStarted records the start timestamp for the job's download.
func (j *Job) MarkJobStarted(t time.Time) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	j.progress.setDownloadStartedOnce(t)
	return nil
}

// RecordDownload adds bytes to the running total for one server.
func (j *Job) RecordDownload(server string, bytes int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if j.progress.serverStats == nil {
		j.progress.serverStats = make(map[string]int64)
	}
	j.progress.serverStats[server] += int64(bytes)
	return nil
}

// MarkDownloadFinished records the time the job left Fetching: its first
// article's start to this stamp is the download duration history shows. The
// first call after the slot is open wins. Its production callers are the
// application's reportDownloadComplete, on the report that moves a complete job
// out of Fetching, and enqueuePostProc, for a job handed to post-processing from
// Fetching before that report: `git grep -n '[M]arkDownloadFinished(' -- '*.go'
// ':!*_test.go'` returns 3 lines, those two calls and this declaration.
func (j *Job) MarkDownloadFinished(t time.Time) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	j.progress.setDownloadFinishedOnce(t)
	return nil
}

// ClearDownloadFinished reopens the finish slot, for a job going back to
// Fetching. Its one production caller is the application's appRunner.advance, on the
// Assessing -> Fetching demotion: `git grep -n '[C]learDownloadFinished()' --
// '*.go' ':!*_test.go'` returns 2 lines, that call and this declaration.
func (j *Job) ClearDownloadFinished() error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	j.progress.clearDownloadFinished()
	return nil
}

// RestoreProgressState applies progress-tier state recovered from the store.
//
// It writes to whichever record is authoritative. Its sole production caller
// is dispatch.restoreJobMetadata (`git grep -n 'j\.RestoreProgressState(' --
// 'internal/dispatch/*.go' ':!*_test.go'` finds one line), which runs
// immediately after reconstruct and therefore always on a job whose progress is
// nil, so the live-record branch is unreachable in production today.
//
// That branch is still not defensive padding: without it a call made after
// hydration would land in fields the accessors stop reading once progress is
// non-nil and return successfully having discarded the value — the identical
// silent drop this function exists to remove, one level up. It adds no writer
// of those fields; restoreDownloadStamps, restorePar2ReleaseReason and
// restorePar2Recovered already own them and it calls all three.
//
// Both branches put the timestamps through jobStampOrZero, so a stamp is
// accepted or rejected the same way whichever record receives it. Without that
// the pre-hydration accessor could report a stamp that hydration then dropped.
//
// AttachContent seeds the fresh JobProgress from the Job-level copy and zeroes it.
func (j *Job) RestoreProgressState(reason string, started, finished time.Time, recovered bool) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress != nil {
		j.progress.restoreDownloadStamps(started, finished)
		j.progress.restorePar2ReleaseReason(reason)
		j.progress.restorePar2Recovered(recovered)
		return
	}
	j.restoredPar2Reason = reason
	j.restoredDLStarted = jobStampOrZero(started)
	j.restoredDLFinished = jobStampOrZero(finished)
	j.restoredPar2Recovered = recovered
}

// Par2Recovered reports whether on-demand par2 un-deferred this job's recovery
// volumes. Reads the restored value before hydration, as Par2ReleaseReason does.
func (j *Job) Par2Recovered() bool {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return j.restoredPar2Recovered
	}
	return j.progress.Par2Recovered()
}
