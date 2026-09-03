package job

import (
	"errors"
	"fmt"
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

// Progress returns the always-resident progress record, or nil before any
// content has been attached.
func (j *Job) Progress() *JobProgress {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.progress
}

// withProgress runs fn under the write lock with the progress record, and is
// the ONLY way the mutators below reach it. Every article-accounting mutation
// in this package goes through here, which is what replaces the 84 direct
// reach-ins internal/queue had into JobProgress's unexported surface.
func (j *Job) withProgress(fn func(*JobProgress) error) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	return fn(j.progress)
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
	j.progress.markFailed(j.manifest, artIdx)
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
