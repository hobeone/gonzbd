package queue

import (
	"context"
	"fmt"

	"github.com/hobeone/gonzbd/internal/durability"
)

// AckDurable resolves the articles a completed fsync covers.
//
// It takes a durability.DurableProof rather than a slice of indices, and that
// is the whole point. DurableProof has no exported constructor outside
// internal/durability, so no code path that has not run a barrier can reach
// this method WITH ANY ARTICLE IN HAND — R9 is enforced by the compiler rather
// than by six call sites each remembering it (X3).
//
// The bound is on the payload, not on the type: `durability.DurableProof{}`
// compiles in any package. The `len(arts) == 0` early return below is what
// makes such a proof inert, so it is part of the invariant rather than a
// defensive nicety. See the DurableProof type doc.
//
// This gate covers THIS door only. SeedFromExtents and ReplaceFromResume also
// reach markDone without a proof, deliberately — their evidence is stable
// storage, which a proof cannot express. Do not read "AckDurable is
// proof-gated" as "nothing marks an article done without a barrier".
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
// It is ADDITIVE, deliberately and permanently: it only ever SETS a bit. An
// article this does not name keeps whatever state it already had, which for a
// restored job is whatever job_files.articles_done recorded.
//
// That is the right contract for this method's one caller and the wrong one
// for the other, which is why there are two. Application.reevaluateStall's
// phase 3 replays extents LOADED FROM THE STORE after a stall recovery: it is
// re-delivering an ack whose fsync already landed, and it has verified nothing
// about any file. A clear there would discard the acks this process made since
// the last commit — precisely the bits that phase exists to preserve.
//
// The startup resume sweep is the caller that HAS just read the file's bytes,
// and it uses ReplaceFromResume instead. Do not merge the two back into one
// entry point, with or without a flag: the union of the two contracts is
// either #362 (a stale bit outliving the recomputation that disproved it) or a
// stall recovery that throws away live acks. TestSeedFromExtents_StaysAdditive
// is the guard on this half.
//
// Two indexing rules make this safe, and both are easy to get wrong in a way
// no range check catches. They are shared with ReplaceFromResume through
// fileDurableBitmap; see its doc.
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
	for _, e := range exts {
		bm, lo, n, err := fileDurableBitmap(m, e)
		if err != nil {
			return fmt.Errorf("queue: SeedFromExtents %s: %w", jobID, err)
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

// ReplaceFromResume installs what a fresh resume PROVED about a job's files,
// in place of what was recorded about them. It is the authoritative half of
// the pair SeedFromExtents documents, and it closes #362.
//
// # Why an authority is needed at all
//
// Store.RestoreJobProgress marks done every article in job_files.articles_done
// before any of this runs, and that column is a BELIEF a previous process
// wrote. durability.Resumer answers the same question from the file's bytes,
// and S4 makes its answer correct by definition: "where it disagrees with a
// recomputation, the recomputation is correct". With only an additive entry
// point the belief always won, so a truncated or deleted partial finished as a
// complete file with a zero-filled hole in it and no warning (#362).
//
// # What it replaces, and what it deliberately does not
//
// For every file named by an extent, an article the resume did not verify goes
// back to Outstanding — S3's absence of evidence read as absence, rather than
// as evidence. ResumeResult.Restart needs no case of its own: a missing file
// yields an empty bitmap, and an empty bitmap already says "nothing here is
// proven".
//
// Two things are NOT touched, and both are limits on what the caller's
// evidence covers rather than concessions:
//
//   - A file with no extent in exts keeps its state entirely. The startup
//     sweep omits a file it never resumed — one whose name was never resolved,
//     one it did not reach before a storage fault, and every file of a job
//     past downloading — and an omission is silence, not a finding of absence.
//   - A permanently failed article is never cleared. See markNotDone: its
//     bytes were never on disk, so their absence is the recorded outcome and
//     not new information.
//
// # A file whose bytes no longer support it does not stay Complete
//
// Complete means "the assembler is finished with this file", NOT "every
// article arrived" — internal/app/pipeline.go hands a permanently failed
// article to the assembler, which closes the file with a gap. So Complete
// cannot be re-derived from the article bits, and it is cleared on exactly the
// evidence this method has: the flag is dropped for a file where a bit was
// actually CLEARED, and left alone otherwise. A Complete file whose successful
// articles all verify keeps its flag even though a failed article is
// permanently missing, which is the case the naive "Complete implies fully
// populated" rule would re-download on every restart.
//
// This mirrors what the retry path already does when it resets a file's failed
// articles (see Job.ResetForRetry, which clears FileProgress.Complete for every
// file it reopens), for the same reason: an assembler that
// must write more bytes into a file is not finished with it.
//
// AssembledCRC32 goes with the flag. It is the combined CRC of a whole
// assembled file, and a file that has lost bytes is not that file any more;
// leaving it would hand postproc's QuickCheck a checksum describing bytes that
// are no longer there. Zero is its documented "unavailable" value (#349), so
// clearing it costs a par2 verify rather than producing a wrong verdict.
//
// # The derived figures
//
// recompute re-derives pendingArticles, articlesResolved, articlesFailed,
// failedBytes and every per-file byte figure from the bitmaps, so the job's
// reported health matches its per-article state. Clearing bits without it is
// the half-inverse that produced #300 from the other direction. It runs
// unconditionally rather than only when something changed: after a pure
// markDone pass it is a no-op by construction, and a condition whose two arms
// agree is untestable branching.
func (q *Queue) ReplaceFromResume(jobID string, exts []durability.FileExtent) error {
	if len(exts) == 0 {
		return nil
	}
	q.mu.Lock()
	// A PAUSED job is not resident, and it is exactly the job that needs this.
	//
	// The startup sweep used to skip it, so #362 survived in that branch: its
	// disproven Done bits were never corrected, the next checkpoint re-committed
	// them from the stored bitmap priorExtent ORs into, and the file finalized
	// over a hole. It also defeated stallLost's own "restart gonzbd to resume
	// this job" instruction, since Stall leaves the job Paused.
	//
	// Hydrated here for the duration and evicted again before returning, so
	// the job's residency is unchanged from outside. Startup is the moment
	// this is cheapest and safest: nothing else holds a manifest and no
	// article is being dispatched.
	job, err := q.residentJob(jobID)
	var evictAfter *Job
	if err != nil {
		j, ok := q.byID[jobID]
		if !ok {
			q.mu.Unlock()
			return fmt.Errorf("queue: ReplaceFromResume %s: %w", jobID, err)
		}
		if hErr := q.hydrateJobLocked(j, jobID); hErr != nil { //lockio: reads the manifest for a non-resident job; startup only, see the comment above
			q.mu.Unlock()
			return fmt.Errorf("queue: ReplaceFromResume %s: %w", jobID, hErr)
		}
		if j.manifest == nil {
			q.mu.Unlock()
			return fmt.Errorf("queue: ReplaceFromResume %s: %w", jobID, err)
		}
		job, evictAfter = j, j
	}
	m := job.manifest
	var cleared int
	for _, e := range exts {
		bm, lo, n, bmErr := fileDurableBitmap(m, e)
		if bmErr != nil {
			q.mu.Unlock()
			return fmt.Errorf("queue: ReplaceFromResume %s: %w", jobID, bmErr)
		}
		fileCleared := 0
		for ord := range n {
			if bm.Get(ord) {
				job.progress.markDone(m, lo+ord)
				continue
			}
			if job.progress.markNotDone(lo + ord) {
				fileCleared++
			}
		}
		if fileCleared > 0 {
			fp := &job.progress.files[int(e.FileIdx)]
			fp.Complete = false
			fp.AssembledCRC32 = 0
			cleared += fileCleared
		}
	}
	job.progress.recompute(m)
	q.dirty.Store(true)

	// A cleared bit must reach the store before this returns.
	//
	// Marking the dirty flag is not enough. Every re-hydration in this package
	// re-reads job_files.articles_done unconditionally — PromoteNext,
	// hydrateSnapshot, SetStatus — so until the row is rewritten, any eviction
	// and re-promotion refills the corrected progress from the pre-correction
	// row and the disproven article is Done again, carrying that file's
	// Complete flag and AssembledCRC32 back with it. The first periodic save
	// can be a whole checkpoint interval away.
	//
	// The window is reached without any concurrency: resumeAllJobs calls this
	// and then Stall on the same job whenever another of its files faulted,
	// and Stall pauses the job, which evicts the manifest with the correction
	// still in memory only.
	//
	// Only when something was CLEARED. A bit this sweep merely SET can be lost
	// to the same window at the cost of a re-fetch, which is the safe
	// direction under S3; a bit it cleared is the direction that finalizes a
	// file over bytes that are not there.
	//
	// Queue.Retry carries the same guard against the same re-read, for the
	// same reason (#260); this generalizes it to the authoritative sweep.
	var persistErr error
	if cleared > 0 && q.store != nil {
		persistErr = q.store.Update(context.Background(), job) //lockio: persists the cleared articles_done before any re-hydration can re-read the stale row
	}
	// Put the job back the way it was found. Done AFTER the persist, so the
	// corrected row is written from the hydrated job rather than from one this
	// call has already torn down.
	if evictAfter != nil {
		q.evictJobLocked(evictAfter)
	}
	q.mu.Unlock()
	// --- No lock held below this line ---
	if cleared > 0 {
		// Never silent (A2): this is the one place a job loses ground it had
		// recorded, and the operator's copy of the file changed underneath it.
		q.log.Warn("resume disproved articles a previous run recorded as downloaded; they will be re-fetched",
			"job", jobID, "articles_cleared", cleared)
	}
	if persistErr != nil {
		// Surfaced rather than logged. The caller's own correction is not
		// durable, so reporting success would let the sweep move on believing
		// ground it has already lost is safe. resumeAllJobs treats a failure
		// here as "the job re-fetches what it could not be told it has", which
		// is the safe direction; silently succeeding is not.
		return fmt.Errorf("queue: ReplaceFromResume %s: persist cleared articles: %w", jobID, persistErr)
	}
	return nil
}

// fileDurableBitmap re-derives one extent's durable bitmap at the file's true
// article count, and returns it with the file's first global article index and
// its article count.
//
// Two indexing rules live here because both seeding entry points need them and
// both are easy to get wrong in a way no range check catches:
//
//   - FileExtent.Durable is indexed by FILE-LOCAL ordinal, while JobProgress
//     is indexed globally. The conversion is the file's own manifest range, so
//     it is taken from FileRange(fileIdx) here rather than passed in.
//   - The bitmap is re-derived at the file's true article count via
//     BitmapFromBytes rather than read at the width persistence rounded it up
//     to. ExtentStore rebuilds each bitmap at its full BYTE width, which is
//     always a multiple of 64, so Bitmap's tail-word mask never fires and
//     padding bits in a damaged blob would otherwise read as durable articles.
//     Over-reporting durability is the over-claim direction the design
//     forbids, and this is the only layer that knows the real count.
func fileDurableBitmap(m *Manifest, e durability.FileExtent) (durable durability.Bitmap, firstArtIdx, artCount int, err error) {
	fi := int(e.FileIdx)
	nFiles := m.NumFiles()
	if fi < 0 || fi >= nFiles {
		return durability.Bitmap{}, 0, 0, fmt.Errorf("file index %d out of range (%d files)", fi, nFiles)
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
		return durability.Bitmap{}, 0, 0, fmt.Errorf("file %d: %w", fi, err)
	}
	return bm, lo, n, nil
}
