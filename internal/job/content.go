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
//
// RestoreContent is the only other WRITER of the pair, and it is not a second
// constructor: it installs progress that already exists, and refuses a pair
// whose two halves describe different jobs.
func (j *Job) AttachContent(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("job %s: AttachContent: nil manifest", j.id)
	}
	// Read m's totals before taking the lock: a Manifest is immutable after
	// construction, so this keeps the critical section to the assignments.
	p := newJobProgress(m)

	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.manifest = m
	j.progress = p
	j.setScalarsLocked(m)
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
	j.setScalarsLocked(m)
	return nil
}

// setScalarsLocked copies m's five totals onto j. Must hold contentMu for
// writing.
//
// It is unexported and lock-suffixed so that the scalars have exactly the two
// writers the pair has. internal/queue kept a separate exported-ish setter and
// a review of it caught two sites that installed a manifest without calling it,
// leaving all five reading zero with a manifest in hand; here there is nowhere
// else to install a manifest from.
func (j *Job) setScalarsLocked(m *Manifest) {
	j.totalBytes = m.TotalBytes()
	j.numFiles = m.NumFiles()
	j.numArticles = m.NumArticles()
	j.recoveryBytes = m.RecoveryBytes()
	j.recoveryFiles = m.RecoveryFiles()
}

// Evict drops the manifest and keeps the progress record.
//
// Progress is always resident by design (docs/queue-lifecycle.md's three
// tiers): it is small, and the abort checks and the queue listing read it for
// jobs that are not running. Only the manifest, which is sized by article
// count, is evictable. The five scalars survive with progress, which is what
// lets a reporting path answer for an evicted job.
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
//
// It returns an error rather than a nil pointer so that depending on an
// evictable manifest is a compile error until the absent case is handled.
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
//
// contentMu guards which *JobProgress this Job points to, not that record's
// contents. Mutations go through the doors below, which hold the write lock;
// a caller reading through the returned pointer while one is in flight still
// races on the individual fields.
func (j *Job) Progress() *JobProgress {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.progress
}

// TotalBytes returns the job's total size in bytes. Never requires a resident
// manifest.
func (j *Job) TotalBytes() int64 {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.totalBytes
}

// NumFiles returns the job's file count. Never requires a resident manifest.
func (j *Job) NumFiles() int {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.numFiles
}

// NumArticles returns the job's article count. Never requires a resident
// manifest.
func (j *Job) NumArticles() int {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.numArticles
}

// RecoveryBytes returns the summed size of the job's par2 recovery volumes,
// excluding the always-downloaded par2 index. See Manifest.RecoveryBytes for
// what this figure can and cannot prove about repairability. Never requires a
// resident manifest.
func (j *Job) RecoveryBytes() int64 {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.recoveryBytes
}

// RecoveryFiles returns the job's par2 recovery volume count, excluding the
// index. Never requires a resident manifest.
func (j *Job) RecoveryFiles() int {
	j.contentMu.RLock()
	defer j.contentMu.RUnlock()
	return j.recoveryFiles
}

// withProgress runs fn under the write lock with the progress record, and is
// the ONLY way the mutators below reach it. Every article-accounting mutation
// in this package goes through here, which is what replaces the direct
// reach-ins internal/queue had into JobProgress's unexported surface.
//
// fn must not call back into any *Job method: contentMu is not reentrant, and
// no *JobProgress method takes a lock of its own. It must also not take j.mu —
// see mu's LOCK ORDER note.
func (j *Job) withProgress(fn func(*JobProgress) error) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	return fn(j.progress)
}

// withResidentContent runs fn under the write lock with both halves of the
// pair, for the mutators that need the manifest as well — the byte arithmetic
// markDone and markFailed perform is per-article, and article sizes live only
// in the manifest.
//
// It is the residency gate in its Job-level form: a mutation that needs the
// manifest reports ErrNotResident rather than silently skipping the job.
func (j *Job) withResidentContent(fn func(*Manifest, *JobProgress) error) error {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.manifest == nil || j.progress == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
	return fn(j.manifest, j.progress)
}

// MarkArticleDone records a successfully downloaded article.
//
// Idempotent: markDone early-returns on an already-done article before
// touching any byte counter, which is what lets an at-least-once durability
// proof be replayed without double-counting.
func (j *Job) MarkArticleDone(artIdx int) error {
	return j.withResidentContent(func(m *Manifest, p *JobProgress) error {
		if artIdx < 0 || artIdx >= m.NumArticles() {
			return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
		}
		p.markDone(m, artIdx)
		return nil
	})
}

// MarkArticleFailed records an article that will not be retried.
func (j *Job) MarkArticleFailed(artIdx int) error {
	return j.withResidentContent(func(m *Manifest, p *JobProgress) error {
		if artIdx < 0 || artIdx >= m.NumArticles() {
			return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
		}
		p.markFailed(m, artIdx)
		return nil
	})
}

// MarkArticleEmitted records that an article has been handed to the downloader.
func (j *Job) MarkArticleEmitted(artIdx int) error {
	return j.withResidentContent(func(m *Manifest, p *JobProgress) error {
		if artIdx < 0 || artIdx >= m.NumArticles() {
			return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
		}
		p.markEmitted(m, artIdx)
		return nil
	})
}

// ClearArticleEmitted undoes MarkArticleEmitted for a work item that was never
// dispatched.
//
// clearEmitted only restores the article to pending if it is not already done:
// an article can be Emitted and Done at once when a durability ack landed
// first, and is finished then.
func (j *Job) ClearArticleEmitted(artIdx int) error {
	return j.withResidentContent(func(m *Manifest, p *JobProgress) error {
		if artIdx < 0 || artIdx >= m.NumArticles() {
			return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
		}
		p.clearEmitted(m, artIdx)
		return nil
	})
}

// MarkFileComplete records that a file's articles have all been assembled.
//
// It needs the manifest only to bound fileIdx, but it needs it for that: the
// progress record's own file slice is sized from the same source, and bounding
// against the manifest is what keeps the two from ever disagreeing about how
// many files a job has.
func (j *Job) MarkFileComplete(fileIdx int) error {
	return j.withResidentContent(func(m *Manifest, p *JobProgress) error {
		if fileIdx < 0 || fileIdx >= m.NumFiles() {
			return fmt.Errorf("job %s: fileIdx %d out of range", j.id, fileIdx)
		}
		p.files[fileIdx].Complete = true
		return nil
	})
}
