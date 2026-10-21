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

// AttachFiles installs the file table and derives the progress record from it.
//
// It is the sole path that DERIVES a fresh progress record: progress is
// computed here from ft and never supplied by a caller, which is what makes
// "progress describes this table" hold for a job starting out rather than
// being merely asserted.
//
// It is the only writer of the file-table pointer and of the progress pointer,
// which TestFilesPointerWriters_MatchTheEnumerationStatedInProse and
// TestProgressPointerWriters_MatchTheEnumerationStatedInProse pin.
// AttachArticles, below, installs a manifest beside the progress record the
// job already holds and never replaces that record.
//
// seed is nil for a freshly ingested job (every file Verified) and, for the
// restore pass, one FileSeed per file (every file !Verified); a seed of any
// other length is refused with nothing attached. No seed sets an article bit.
//
// Non-test callers: AttachContent (below) and Application.restoreProgress
// (internal/app/restore_progress.go) — `git grep -n 'AttachFiles(' -- '*.go' ':!*_test.go'`
// returns 4 lines, those two calls, this declaration and this sentence.
func (j *Job) AttachFiles(ft *FileTable, seed []FileSeed) error {
	if ft == nil {
		return fmt.Errorf("job %s: AttachFiles: nil file table", j.id)
	}
	if seed != nil && len(seed) != ft.NumFiles() {
		return fmt.Errorf("job %s: AttachFiles: seed has %d entries for a table of %d files", j.id, len(seed), ft.NumFiles())
	}
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	// Refuse a second attach rather than silently replacing a live record.
	// newJobProgress below builds a FRESH JobProgress, so a re-attach would
	// discard every done/failed bit and both stamps with no error — and since
	// the first attach zeroed the Job-level copy, there would be nothing left
	// to seed the replacement from either. Callers attach once: the restore
	// pass (Application.restoreProgress) refuses a job that already has a
	// table, the ingest path builds a fresh Job, and residency.Hydrate never
	// calls AttachFiles. That is what makes this an assertion of an invariant
	// rather than a behaviour change.
	if j.progress != nil || j.fileTable != nil {
		return fmt.Errorf("job %s: AttachFiles: files already attached; use AttachArticles to install a manifest alongside existing progress", j.id)
	}
	j.fileTable = ft
	j.progress = newJobProgress(ft, seed)
	j.totalBytes = ft.TotalBytes()
	j.recoveryBytes = ft.RecoveryBytes()
	j.recoveryFiles = ft.RecoveryFiles()
	// Seed from state restored before this job had a JobProgress, then zero it
	// so the Job-level copy and the progress record are never both live. These
	// go through the existing restore doors rather than assigning the fields:
	// restoreDownloadStamps owns the isJobStamp filter, and a second
	// implementation of it here is exactly the divergence that filter exists
	// to prevent. Direct field access, not the exported setters — contentMu is
	// held and sync.RWMutex is not reentrant.
	j.progress.restoreDownloadStamps(j.restoredDLStarted, j.restoredDLFinished)
	j.progress.restorePar2ReleaseReason(j.restoredPar2Reason)
	j.restoredPar2Reason = ""
	j.restoredDLStarted = time.Time{}
	j.restoredDLFinished = time.Time{}
	return nil
}

// AttachArticles installs the article table beside the job's file table and
// its own progress record. It is the counterpart of Evict, and the only
// writer of the manifest pointer besides Evict's nil, which
// TestManifestPointerWriters_MatchTheEnumerationStatedInProse pins.
//
// The record is recomputed in place, under the same lock its writers take, and
// never replaced. Writes that need no manifest — MarkArticleFailed,
// ClearArticleEmitted and ClearEmittedForReload while evicted, and the
// progress-tier setters — land on
// that record at any time, so a copy taken earlier and installed here would
// silently discard whatever arrived in between. recompute is what folds the
// bits those writes left into the counters.
//
// It verifies the manifest was built from this job's own table, by pointer
// identity rather than by comparing shapes: NewManifestFromArticles already
// refused a count that disagrees with its table, so the pairing is the only
// thing left to check.
func (j *Job) AttachArticles(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("job %s: AttachArticles: nil manifest", j.id)
	}
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.fileTable == nil {
		return fmt.Errorf("job %s: AttachArticles: no file table attached; use AttachFiles", j.id)
	}
	if m.Files() != j.fileTable {
		return fmt.Errorf("job %s: AttachArticles: manifest was built from a different file table", j.id)
	}
	if j.manifest != nil {
		return fmt.Errorf("job %s: AttachArticles: manifest already attached", j.id)
	}
	j.progress.recompute(m)
	j.manifest = m
	return nil
}

// AttachContent attaches a job's files and articles together: AttachFiles on
// m's table, then AttachArticles on m. It writes no field itself.
// It takes contentMu twice, so a reader between the two calls sees a job with
// a table and progress but no manifest, which looks evicted; harmless for
// ingest, which attaches before the job is visible. Hydrate does not call it:
// the job already holds its table, and Hydrate calls AttachArticles alone,
// under the hydrating claim it takes at `r.hydrating.TryClaim(id)` in
// internal/app/residency.go.
func (j *Job) AttachContent(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("job %s: AttachContent: nil manifest", j.id)
	}
	if err := j.AttachFiles(m.Files(), nil); err != nil {
		return err
	}
	return j.AttachArticles(m)
}

// AllVerified reports whether every file's verification is installed
// (JobProgress.AllVerified); false when no progress record is attached.
func (j *Job) AllVerified() bool {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.progress.AllVerified()
}

// Files returns the job's file table, or nil before AttachFiles.
func (j *Job) Files() *FileTable {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.fileTable
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
// zero if unstarted. While the job has no JobProgress it reads the value
// restored from the store, so a restored job reports the same stamp before and
// after AttachFiles seeds the record from it.
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
// reports it too; ResetForRetry does not clear it, and post-processing rename
// stages update it in place (RecordFileRename). The writers of
// FileProgress.Filename are those three —
// `git grep -n '\]\.Filename = ' -- 'internal/job/*.go' ':!*_test.go'` returns 3 lines.
//
// While the job has no JobProgress it reads the restored start stamp, which a
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
// unfinished. Reads the restored value while the job has no JobProgress, as
// DownloadStarted does.
func (j *Job) DownloadFinished() time.Time {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return j.restoredDLFinished
	}
	return j.progress.DownloadFinished()
}

// Par2ReleaseReason returns the reason par2 articles were released, or empty
// if not released. Reads the restored value while the job has no JobProgress,
// as DownloadStarted does.
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
// restore, undeferRecovery, DiscardDeferredPar2 and newJobProgress (a restore
// seed's policy) all call it. A progress built with a nil seed starts every
// file at the FetchAlways zero; a restore seed's policy is applied through
// this function. The `job_files.fetch_policy` CHECK (0-2) is the only range
// guard a restored value has. The caller holds contentMu and has
// range-checked fi.
//
// It owns what the policy decides about the pending counters: a file counts
// pending work only while FetchAlways (countsPending), so it re-derives the
// file's Pending from its bits (pendingOf) in the same critical section. That
// needs no manifest, so it holds at any residency, and the four callers named
// above leave the counters to it (#903).
//
// It marks the file for Job.TakeFetchPolicyChanges whenever the stored value
// differs from the previous one, so the record learns of every change without
// its caller having to say so.
func (p *JobProgress) setFileFetchPolicy(fi int, policy FetchPolicy) {
	f := &p.files[fi]
	if f.Fetch == policy {
		return
	}
	f.Fetch = policy
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
// records the bits and the charge alone: the counters need the manifest's
// file ranges and article sizes, and
// AttachArticles's recompute derives the counters from the bits when the job
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
	// A failure changes no fetch policy: the Assessing verdict sizes any
	// recovery release from the written rows once the content is down.
	j.progress.markFailedUncovered(j.manifest, artIdx, charge)
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
// Evicted, it clears the bit alone, and AttachArticles's recompute restores the
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
			if !fn(fi, int32(i), m.ArticleID(i), m.ArticleBytes(i), m.ArticleNumber(i), m.Files().Subject(fi)) {
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
	return j.repairStateLocked()
}

// repairStateLocked is the one place a Job's RepairState is computed from its
// own figures. The caller holds contentMu, read or write.
func (j *Job) repairStateLocked() RepairState {
	if j.progress == nil {
		return RepairUnknown
	}
	return RepairStateFrom(j.progress.ContentFailedBytes(), j.recoveryBytes, j.progress.HasPar2Files())
}

// NumFiles returns the number of files in the job if files have been attached,
// or 0 before AttachFiles. It reads the file table, then progress; never the
// manifest, so it answers for an evicted job.
func (j *Job) NumFiles() int {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.fileTable != nil {
		return j.fileTable.NumFiles()
	}
	if j.progress != nil {
		return len(j.progress.files)
	}
	return 0
}

// undeferRecovery releases each held volume of fileIdxs to FetchAlways. The
// caller holds contentMu and the job has progress.
func (j *Job) undeferRecovery(fileIdxs []int) {
	for _, fi := range fileIdxs {
		if fi < 0 || fi >= len(j.progress.files) || j.progress.files[fi].Fetch != FetchIfNeeded {
			continue
		}
		j.progress.setFileFetchPolicy(fi, FetchAlways)
	}
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
// did before #504 — would make a write-then-read on a job that has no
// JobProgress return the restored value instead of the one just written. Writing
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

// ClearEmittedForReload resets Emitted and Failed flags for reload
// (resetForReload, per file).
//
// It does not require the manifest. A job evicted when the reload runs still
// holds the failures the old servers recorded, through MarkArticleFailed at
// any residency, and skipping it would keep them failed for the rest of the
// process (#904). Evicted, it writes the bits alone, and AttachArticles's
// recompute derives the counters at the next hydration. Resident, it
// recomputes here, whatever the reset touched: a cleared emitted bit returns
// its article to Pending as surely as an un-failed one does.
func (j *Job) ClearEmittedForReload(skipEmitted bool) (cleared, retained []int32) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return nil, nil
	}
	for fi := range j.progress.files {
		c, r := j.progress.resetForReload(fi, !skipEmitted)
		cleared = append(cleared, c...)
		retained = append(retained, r...)
	}
	if j.manifest != nil {
		j.progress.recompute(j.manifest)
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
// retried a restored job in place before its first hydration would return
// early here on the manifest == nil guard and keep the old progress-tier
// values.
func (j *Job) ResetForRetry() {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return
	}
	j.progress.earlyAborted = false
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
//
// A job whose failed content fits its par2 recovery capacity (RepairIntact or
// RepairPossible) is spared, and the latch stays unset so a later failure can
// still trigger the rule once the figures change. The rate rule therefore
// applies only when capacity is absent (RepairNoCapacity), exceeded
// (RepairBeyondCapacity) or unmeasured (RepairUnknown). For the first two the
// downloader's Early Health Gate, which acts on Hopeless(), reaches the same
// verdict, so the rate is redundant there; it decides on its own only for
// RepairUnknown (#1011).
func (j *Job) CheckEarlyAbort() bool {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return false
	}
	if st := j.repairStateLocked(); st == RepairPossible || st == RepairIntact {
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
// AttachFiles would land in fields the accessors stop reading once progress is
// non-nil and return successfully having discarded the value — the identical
// silent drop this function exists to remove, one level up. It adds no writer
// of those fields; restoreDownloadStamps and restorePar2ReleaseReason already
// own them and it calls both.
//
// Both branches put the timestamps through jobStampOrZero, so a stamp is
// accepted or rejected the same way whichever record receives it. Without that
// the accessor could report a stamp that AttachFiles then dropped.
//
// AttachFiles seeds the fresh JobProgress from the Job-level copy and zeroes it.
func (j *Job) RestoreProgressState(reason string, started, finished time.Time) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress != nil {
		j.progress.restoreDownloadStamps(started, finished)
		j.progress.restorePar2ReleaseReason(reason)
		return
	}
	j.restoredPar2Reason = reason
	j.restoredDLStarted = jobStampOrZero(started)
	j.restoredDLFinished = jobStampOrZero(finished)
}
