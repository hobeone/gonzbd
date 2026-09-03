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
// It is the SOLE constructor of the (manifest, progress) pair. Progress is
// derived here and never supplied by a caller, which is what makes
// "progress describes this manifest" an invariant rather than a comment: there
// is no second path that could pair a manifest with progress for another job.
func (j *Job) AttachContent(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("job %s: AttachContent: nil manifest", j.id)
	}
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.manifest = m
	j.progress = newJobProgress(m)
	return nil
}

// RestoreContent installs a manifest together with progress recovered from the
// store. It is AttachContent's counterpart for a job that has run before, and
// it is the only other writer of the pair.
//
// It verifies the two describe the same job rather than trusting the caller:
// describesSameJobAs compares article and file counts, and a mismatch here
// means the stored progress belongs to a different manifest revision.
func (j *Job) RestoreContent(m *Manifest, p *JobProgress) error {
	if m == nil || p == nil {
		return fmt.Errorf("job %s: RestoreContent: nil manifest or progress", j.id)
	}
	if !p.describesSameJobAs(m) {
		return fmt.Errorf("job %s: RestoreContent: progress describes a different manifest", j.id)
	}
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	p.recompute(m)
	j.manifest = m
	j.progress = p
	return nil
}

// Evict drops the manifest and keeps the progress record.
//
// Progress is always resident by design (docs/queue-lifecycle.md's three
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
	j.progress.files[fi].Fetch = policy
	return nil
}

// MarkArticleDone records a successfully downloaded article.
func (j *Job) MarkArticleDone(artIdx int, bytes int64, server string) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil || j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if artIdx < 0 || artIdx >= j.manifest.NumArticles() {
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
	j.progress.markDone(j.manifest, artIdx)
	return nil
}

// MarkArticleFailed records an article that will not be retried.
func (j *Job) MarkArticleFailed(artIdx int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil || j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if artIdx < 0 || artIdx >= j.manifest.NumArticles() {
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
	if j.progress.markFailed(j.manifest, artIdx) {
		if !j.progress.par2Recovered && j.manifest.RecoveryFiles() > 0 {
			if j.undeferRecovery(j.progress.DeferredRecoveryIndices()) {
				j.progress.par2ReleaseReason = "permanent article download failure detected on active queue"
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

// ClearArticleEmitted undoes MarkArticleEmitted for a work item that was never
// dispatched.
func (j *Job) ClearArticleEmitted(artIdx int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil || j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if artIdx < 0 || artIdx >= j.manifest.NumArticles() {
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
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
func (j *Job) MarkFileComplete(fileIdx int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	return j.markFileComplete(fileIdx)
}

func (j *Job) markFileComplete(fileIdx int) error {
	if j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= j.manifest.NumFiles() {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Complete = true
	return nil
}

// RecoveryBytes returns the summed size of the job's par2 recovery volumes,
// or 0 if non-resident.
func (j *Job) RecoveryBytes() int64 {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.manifest == nil {
		return 0
	}
	return j.manifest.RecoveryBytes()
}

// RecoveryFiles returns the job's par2 recovery volume count, or 0 if non-resident.
func (j *Job) RecoveryFiles() int {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.manifest == nil {
		return 0
	}
	return j.manifest.RecoveryFiles()
}

// RepairState reports whether this job's damaged content is within its par2
// recovery capacity.
func (j *Job) RepairState() RepairState {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	if j.progress == nil {
		return RepairIntact
	}
	var recBytes int64
	if j.manifest != nil {
		recBytes = j.manifest.RecoveryBytes()
	}
	return RepairStateFrom(j.progress.ContentFailedBytes(), recBytes, j.progress.HasPar2Files())
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

func (j *Job) undeferRecovery(fileIdxs []int) bool {
	changed := false
	for _, fi := range fileIdxs {
		if fi < 0 || fi >= j.manifest.NumFiles() || j.progress.files[fi].Fetch != FetchIfNeeded {
			continue
		}
		j.progress.files[fi].Fetch = FetchAlways
		changed = true
	}
	if changed {
		j.progress.par2Recovered = true
		j.progress.recompute(j.manifest)
	}
	return changed
}

// UndeferRecoveryVolumes marks the specified deferred recovery volumes as
// FetchAlways, re-activating them for download when repair is needed.
func (j *Job) UndeferRecoveryVolumes(fileIdxs []int) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	for _, fi := range fileIdxs {
		if fi < 0 || fi >= j.manifest.NumFiles() {
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

// DiscardDeferredPar2 marks every recovery volume still awaiting the CRC
// verdict as never-fetch, reporting whether any file changed.
func (j *Job) DiscardDeferredPar2() bool {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return false
	}
	changed := false
	for fi := range len(j.progress.files) {
		if j.progress.files[fi].Fetch == FetchIfNeeded {
			j.progress.files[fi].Fetch = FetchNever
			changed = true
		}
	}
	if changed && j.manifest != nil {
		j.progress.recompute(j.manifest)
	}
	return changed
}

// SetPar2ReleaseReason records the reason deferred recovery volumes were released or discarded.
func (j *Job) SetPar2ReleaseReason(reason string) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress != nil {
		j.progress.par2ReleaseReason = reason
	}
}

// AckDurable applies a durability proof's articles, returning how many named
// an article this job does not have, and how many it does have.
func (j *Job) AckDurable(arts []int32) (invalid, nArt int, err error) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return 0, 0, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	nArt = j.manifest.NumArticles()
	for _, idx := range arts {
		i := int(idx)
		if i < 0 || i >= nArt {
			invalid++
			continue
		}
		j.progress.markDone(j.manifest, i)
	}
	return invalid, nArt, nil
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

// SetFileFilename stores the resolved final filename on a file.
func (j *Job) SetFileFilename(fileIdx int, filename string) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= j.manifest.NumFiles() {
		return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	j.progress.files[fileIdx].Filename = filename
	return nil
}

// SetFileCRC32FromRuns stores the assembled CRC32 on a file if runs prove it.
func (j *Job) SetFileCRC32FromRuns(fileIdx int, runs []durability.Run) (bool, error) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return false, fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	if fileIdx < 0 || fileIdx >= j.manifest.NumFiles() {
		return false, fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
	}
	if len(runs) != 1 || runs[0].Offset != 0 {
		return false, nil
	}
	lo, hi := j.manifest.FileRange(fileIdx)
	if int(runs[0].FirstArtIdx) != lo || int(runs[0].LastArtIdx) != hi-1 {
		return false, nil
	}
	j.progress.files[fileIdx].AssembledCRC32 = runs[0].CRC32
	return true, nil
}

// SeedFromRuns marks every article covered by runs as done.
func (j *Job) SeedFromRuns(runs []durability.Run) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	type span struct{ lo, hi int }
	spans := make([]span, 0, len(runs))
	for _, r := range runs {
		lo, hi, err := runsCoverage(j.manifest, r)
		if err != nil {
			return err
		}
		spans = append(spans, span{lo: lo, hi: hi})
	}
	for _, s := range spans {
		for i := s.lo; i <= s.hi; i++ {
			j.progress.markDone(j.manifest, i)
		}
	}
	return nil
}

// ReplaceFromRuns installs what a fresh resume established about a job's
// files, resetting unverified articles to Outstanding.
func (j *Job) ReplaceFromRuns(files []int32, runs []durability.Run) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	m := j.manifest
	covered := make([]bool, m.NumArticles())
	for _, r := range runs {
		lo, hi, cErr := runsCoverage(m, r)
		if cErr != nil {
			return cErr
		}
		for i := lo; i <= hi; i++ {
			covered[i] = true
		}
	}
	for _, fi := range files {
		f := int(fi)
		if f < 0 || f >= m.NumFiles() {
			return fmt.Errorf("job %s: file index %d out of range (%d files)", j.id, f, m.NumFiles())
		}
		lo, hi := m.FileRange(f)
		fileCleared := 0
		for i := lo; i < hi; i++ {
			if covered[i] {
				j.progress.markDone(m, i)
				continue
			}
			if j.progress.markNotDone(i) {
				fileCleared++
			}
		}
		if fileCleared > 0 {
			fp := &j.progress.files[f]
			fp.Complete = false
			fp.AssembledCRC32 = 0
		}
	}
	j.progress.recompute(m)
	return nil
}

// ClearEmittedForReload resets Emitted and Failed flags for reload/restart.
func (j *Job) ClearEmittedForReload(skipEmitted bool) (cleared, retained []int32) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return nil, nil
	}
	m := j.manifest
	for i := range m.NumArticles() {
		switch {
		case j.progress.resetForReload(m, i, !skipEmitted):
			cleared = append(cleared, int32(i))
		case j.progress.failed.Get(i):
			retained = append(retained, int32(i))
		}
	}
	if len(cleared) > 0 || len(retained) > 0 {
		j.progress.recompute(m)
	}
	return cleared, retained
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

// MarkDownloadFinished records the download completion timestamp.
func (j *Job) MarkDownloadFinished(t time.Time) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	j.progress.setDownloadFinishedOnce(t)
	return nil
}

func runsCoverage(m *Manifest, r durability.Run) (first, last int, err error) {
	fi := int(r.FileIdx)
	nFiles := m.NumFiles()
	if fi < 0 || fi >= nFiles {
		return 0, 0, fmt.Errorf("file index %d out of range (%d files)", fi, nFiles)
	}
	lo, hi := m.FileRange(fi)
	first, last = int(r.FirstArtIdx), int(r.LastArtIdx)
	if first > last || first < lo || last >= hi {
		return 0, 0, fmt.Errorf(
			"file %d: run covers articles [%d,%d], outside the file's range [%d,%d)",
			fi, first, last, lo, hi)
	}
	return first, last, nil
}
