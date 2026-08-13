package queue

import (
	"fmt"

	"github.com/hobeone/gonzbd/internal/durability"
)

// AckDurable resolves the articles a completed fsync covers.
//
// It takes a durability.DurableProof rather than a slice of indices, and that
// is the whole point. DurableProof has no exported constructor outside
// internal/durability, so this method is unreachable from any code path that
// has not actually run a barrier — R9 is enforced by the compiler rather than
// by six call sites each remembering it (X3).
//
// Before this design the assembler could ack from six places, each
// independently responsible for knowing that acceptance into a buffer is not
// evidence about disk. That is why the same defect kept being refiled (#355,
// #356). There is now one place, and it is not here: this method applies what
// the barrier decided, it does not decide anything.
//
// The apply is idempotent because markDone is (R12). At-least-once delivery is
// the contract — SyncTarget.Drain is explicitly permitted to re-report an
// article a previous Drain returned — so a replayed proof must not double-count
// bytes. markDone early-returns on an already-done article before touching
// BytesDownloaded, so it does not.
func (q *Queue) AckDurable(p durability.DurableProof) error {
	arts := p.Articles()
	if len(arts) == 0 {
		return nil
	}
	jobID := p.JobID()

	q.mu.Lock()
	job, err := q.residentJob(jobID)
	if err != nil {
		q.mu.Unlock()
		return fmt.Errorf("queue: AckDurable %s: %w", jobID, err)
	}
	nArt := job.manifest.NumArticles()
	var invalidCount int
	for _, idx := range arts {
		i := int(idx)
		if i < 0 || i >= nArt {
			invalidCount++
			continue
		}
		job.progress.markDone(job.manifest, i)
	}
	q.dirty.Store(true)
	q.mu.Unlock()
	// --- No lock held below this line ---
	if invalidCount > 0 {
		// A proof naming an article this job does not have is a numbering
		// defect upstream, not a storage condition. It cannot be silently
		// dropped (A2), and it must not fail the whole ack either: the
		// articles that ARE in range were made durable by a real fsync, and
		// discarding their acks would cost a re-download of bytes already on
		// disk.
		q.log.Warn("AckDurable: out-of-range article index in proof",
			"job", jobID, "invalid_count", invalidCount, "num_articles", nArt)
	}
	return nil
}

// AckPermanentFailure records articles that will never be fetched, on every
// eligible server.
//
// Unlike AckDurable this needs no proof, and R10 says why: a permanent failure
// asserts nothing about disk. There is no fsync for it to be ordered after,
// and losing one in a crash is safe — the restart re-attempts an article that
// fails again, costing one request. The asymmetry is the design's, not an
// oversight: over-fetching is a cost and over-claiming is a defect, so only the
// claim direction needs the compiler's help.
//
// A storage fault must never reach this method. That is A1: ENOSPC or EIO
// resolves against storage and stalls the job with the articles left
// Outstanding, while a missing or corrupt article resolves against the article
// and is counted as damage. Routing a full disk here would burn the article's
// retry budget, inflate the job's failed-byte count and degrade its reported
// health (R21).
func (q *Queue) AckPermanentFailure(jobID string, artIdxs []int32) error {
	if len(artIdxs) == 0 {
		return nil
	}
	q.mu.Lock()
	job, err := q.residentJob(jobID)
	if err != nil {
		q.mu.Unlock()
		return fmt.Errorf("queue: AckPermanentFailure %s: %w", jobID, err)
	}
	nArt := job.manifest.NumArticles()
	var firstTime, invalidCount int
	for _, idx := range artIdxs {
		i := int(idx)
		if i < 0 || i >= nArt {
			invalidCount++
			continue
		}
		if job.progress.markFailed(job.manifest, i) {
			firstTime++
		}
	}
	var failedBytes, recoveryBytes int64
	var releasedPar2 bool
	if firstTime > 0 {
		failedBytes, recoveryBytes = job.progress.failedBytes, job.manifest.RecoveryBytes()
		q.dirty.Store(true)
		// On-demand par2: a permanent data-article failure proves this job
		// will need repair. Release the deferred recovery volumes now — while
		// the connection is live and the articles are freshest — rather than
		// waiting for the download-complete verify.
		if !job.progress.par2Recovered && job.manifest.RecoveryFiles() > 0 {
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
		q.log.Warn("AckPermanentFailure: out-of-bounds article index received",
			"job", jobID, "invalid_count", invalidCount, "num_articles", nArt)
	}
	if firstTime > 0 {
		q.log.Warn("articles marked FAILED", "job", jobID, "count", firstTime,
			"failed_bytes", failedBytes, "recovery_bytes", recoveryBytes)
		if releasedPar2 {
			q.log.Info("on-demand par2: download failure detected, releasing recovery volumes early", "job", jobID)
		}
	}
	return nil
}

// SeedFromExtents installs a resumed job's durable article bits, so a restart
// does not re-fetch bytes an earlier run already got onto stable storage (L3).
//
// The extents are Class B — a cache the barrier committed after the fsync that
// made its claims true — and this is the point at which that cache becomes the
// running job's belief about what is outstanding.
//
// "Anything NOT marked durable here stays Outstanding" is what this doc used to
// claim, and it is FALSE. This function only ever SETS a bit; it clears none.
// By the time it runs, Store.RestoreJobProgress has already marked done every
// article in job_files.articles_done, so an article the caller's recomputation
// has just proved absent from the disk stays done and is never fetched again —
// and the job completes a file with a hole in it. That is #362. S3 is what the
// behaviour SHOULD be here and is not yet; do not read the additive semantics
// as a deliberate expression of it.
//
// The additive semantics are nonetheless right for the OTHER caller.
// Application.reevaluateStall replays extents loaded from the store, not a
// fresh resume, so a clear there would discard acks this process made since the
// last commit. Whatever closes #362 needs a separate entry point rather than a
// change of meaning here.
//
// Two indexing rules make this safe, and both are easy to get wrong in a way
// no range check catches:
//
//   - FileExtent.Durable is indexed by FILE-LOCAL ordinal, while JobProgress
//     is indexed globally. The conversion is the file's own manifest range, so
//     it is taken from FileRange(fileIdx) here rather than passed in.
//   - The bitmap is re-derived at the file's true article count via
//     BitmapFromBytes rather than read at the width persistence rounded it up
//     to. ExtentStore.Load rebuilds each bitmap at its full BYTE width, which
//     is always a multiple of 64, so Bitmap's tail-word mask never fires and
//     padding bits in a damaged blob would otherwise read as durable articles.
//     Over-reporting durability is the over-claim direction the design
//     forbids, and this is the only layer that knows the real count.
func (q *Queue) SeedFromExtents(jobID string, exts []durability.FileExtent) error {
	if len(exts) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	job, err := q.residentJob(jobID)
	if err != nil {
		return fmt.Errorf("queue: SeedFromExtents %s: %w", jobID, err)
	}
	m := job.manifest
	nFiles := m.NumFiles()
	for _, e := range exts {
		fi := int(e.FileIdx)
		if fi < 0 || fi >= nFiles {
			return fmt.Errorf("queue: SeedFromExtents %s: file index %d out of range (%d files)", jobID, fi, nFiles)
		}
		lo, hi := m.FileRange(fi)
		n := hi - lo
		raw := e.Durable.Bytes()
		// A stored bitmap narrower than the file's article count is widened
		// with zeros, which reads as "those articles are not durable yet" —
		// the safe direction under S3. Rejecting it instead would fail a
		// resume over a file whose article count grew, and adopting it at its
		// short width would silently drop the tail articles from the mapping.
		if need := (n + 63) / 64 * 8; len(raw) < need {
			widened := make([]byte, need)
			copy(widened, raw)
			raw = widened
		}
		bm, err := durability.BitmapFromBytes(raw, n)
		if err != nil {
			return fmt.Errorf("queue: SeedFromExtents %s file %d: %w", jobID, fi, err)
		}
		for ord := range n {
			if bm.Get(ord) {
				job.progress.markDone(m, lo+ord)
			}
		}
	}
	q.dirty.Store(true)
	return nil
}
