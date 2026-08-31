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
// This gate covers THIS door only. SeedFromRuns and ReplaceFromRuns also
// reach markDone without a proof, deliberately — their evidence is stable
// storage, which a proof cannot express. Do not read "AckDurable is
// proof-gated" as "nothing marks an article done without a barrier".
//
// Both of those doors are narrower than they were, though, and it is worth
// saying exactly how far that goes. durability.Run is an exported struct with
// exported fields, so ANY package can build one — the narrowing is not that
// the type is unforgeable. It is that a Run carries an article RANGE and a
// byte range that runsCoverage validates against the job's own manifest, where
// the FileExtent these replaced carried a fully exported, settable Bitmap
// whose set bits were taken at face value. A caller that fabricates a Run
// still has to name articles the manifest has.
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
	job, ok := q.byID[jobID]
	if !ok {
		q.mu.Unlock()
		return fmt.Errorf("queue: AckDurable %s: %w: %s", jobID, ErrNotFound, jobID)
	}
	invalidCount, nArt, err := job.ackDurable(arts)
	if err != nil {
		q.mu.Unlock()
		return fmt.Errorf("queue: AckDurable %s: %w", jobID, err)
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
//
// It is the single WRITE site of failed_articles, and its persistence runs
// below the lock rather than inside it, because the project bans I/O under one
// and this method's whole body used to run under q.mu.
//
// Running below the lock is what makes the write raceable against a reversal
// that also runs below the lock, so it takes q.failedPersistMu and re-checks
// the reload generation it captured under q.mu before committing anything.
// Queue's failedPersistMu doc carries the full argument; the short version is
// that a row surviving its own in-memory bit is re-derived as Failed+Done on
// the next promotion and the article is never fetched again.
//
// A persist failure is logged, not returned, and R10 is the licence: a
// permanent failure asserts nothing about disk, and losing one in a crash is
// safe because the restart re-attempts an article that fails again, costing
// one request. That is the same reasoning that lets this method take no proof.
// The REVERSAL is the opposite direction, and the two sites differ. Queue.Retry
// returns its clear's error and rolls the retry back, for the reason #260 gives.
// Queue.ClearAllEmitted's is best-effort like this method, but the failure is
// not symmetric with this one: a row that outlives the clear is reloaded by
// resolutionForJob and re-derives the article as Failed+Done, so it is NOT
// re-attempted after a restart — the reverse of losing a row, which costs one
// request at an article that fails again. See SQLiteStore.ClearFailedArticles
// and ClearFailedArticlesByIdx respectively.
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
	// Only the in-range indices are persisted. An out-of-range one is a
	// numbering defect and a row for it could never be interpreted against any
	// manifest; it is counted and warned about below instead.
	persist := make([]int32, 0, len(artIdxs))
	for _, idx := range artIdxs {
		i := int(idx)
		if i < 0 || i >= nArt {
			invalidCount++
			continue
		}
		persist = append(persist, idx)
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
			if job.undeferRecovery(job.progress.DeferredRecoveryIndices()) {
				job.progress.par2ReleaseReason = "permanent article download failure detected on active queue"
				releasedPar2 = true
			}
		}
		q.notifyLocked()
	}
	// Captured under the same lock hold as the in-memory marks above, so the
	// two cannot straddle a reversal. See the failedPersistMu/failedGen doc on
	// Queue for why an unordered write here is a defect rather than a lost
	// optimisation.
	gen := q.failedGen.Load()
	q.mu.Unlock()
	// --- No lock held below this line ---
	var overtaken bool
	var persistErr error
	if len(persist) > 0 && q.store != nil {
		q.failedPersistMu.Lock()
		// A reversal that ran between the marks above and this write has
		// already cleared the bits these rows would describe. Writing them now
		// would leave rows with nothing in memory behind them, and the next
		// RestoreJobProgress would re-derive the articles as Failed+Done —
		// never re-fetched, which is the outcome the reversal exists to
		// prevent. Dropping the write instead costs one re-request of an
		// article that fails again (R10).
		if overtaken = q.failedGen.Load() != gen; !overtaken {
			persistErr = q.store.RecordFailedArticles(context.Background(), jobID, persist) //lockio: failedPersistMu exists to serialise exactly this call against the reversals in ClearAllEmitted and Retry; see Queue.failedPersistMu
		}
		q.failedPersistMu.Unlock()
	}
	// Reporting is deliberately outside failedPersistMu: the mutex's whole job
	// is ordering the store call against a delete, and a logging handler is
	// neither.
	if overtaken {
		q.log.Info("dropping a permanently-failed-article write that a reset overtook; "+
			"the articles will be re-attempted and fail again",
			"job", jobID, "count", len(persist))
	}
	if persistErr != nil {
		q.log.Warn("could not persist permanently failed articles; they will be "+
			"re-attempted after a restart and fail again",
			"job", jobID, "count", len(persist), "err", persistErr)
	}
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

// SeedFromRuns installs a job's stored durable runs into its live work set, so
// a restart does not re-fetch bytes an earlier run already got onto stable
// storage (L3).
//
// A run is written only after the fsync that makes its bytes durable, and this
// is the point at which that record becomes the running job's belief about
// what is outstanding.
//
// It is ADDITIVE, deliberately and permanently: it only ever SETS a bit. An
// article this does not name keeps whatever state it already had, which for a
// restored job is whatever Store.RestoreJobProgress derived.
//
// That is the right contract for this method's one caller and the wrong one
// for the other, which is why there are two. Application.reevaluateStall's
// phase 3 replays runs LOADED FROM THE STORE after a stall recovery: it is
// re-delivering an ack whose fsync already landed, and it has stat'ed nothing.
// A clear there would discard the acks this process made since the last
// commit — precisely the bits that phase exists to preserve.
//
// The startup resume sweep is the caller that HAS stat'ed each file, and it
// uses ReplaceFromRuns instead. Do not merge the two back into one entry
// point, with or without a flag: the union of the two contracts is either #362
// (a stale bit outliving the check that disproved it) or a stall recovery that
// throws away live acks. TestSeedFromRuns_StaysAdditive is the guard on this
// half, and it reddens when the two are merged.
//
// The indexing rule that makes both safe lives in runsCoverage; see its doc.
func (q *Queue) SeedFromRuns(jobID string, runs []durability.Run) error {
	if len(runs) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	job, ok := q.byID[jobID]
	if !ok {
		return fmt.Errorf("queue: SeedFromRuns %s: %w: %s", jobID, ErrNotFound, jobID)
	}
	if err := job.seedFromRuns(runs); err != nil {
		return fmt.Errorf("queue: SeedFromRuns %s: %w", jobID, err)
	}
	q.dirty.Store(true)
	return nil
}

// ReplaceFromRuns installs what a fresh resume established about a job's
// files, in place of what was recorded about them. It is the authoritative
// half of the pair SeedFromRuns documents, and it closes #362.
//
// # Why an authority is needed at all
//
// Store.RestoreJobProgress derives every article's state from the SAME runs
// before any of this dispatches, so on the ordinary path the two agree. They
// diverge in exactly one case, and it is the case that matters:
// durability.Resumer stats each file and DELETES the runs of a file shorter
// than they claim. The sweep therefore hands back a smaller set than the
// restore installed, and those articles have to go back to Outstanding. With
// only an additive entry point the earlier belief always won, so a truncated
// or deleted partial finished as a complete file with a zero-filled hole in it
// and no warning (#362).
//
// # What it replaces, and what it deliberately does not
//
// For every file the sweep named, an article no surviving run covers goes back
// to Outstanding — S3's absence of evidence read as absence, rather than as
// evidence. ResumeResult.Restart needs no case of its own: a discarded file
// comes back with no runs at all, which already says "nothing here is
// recorded".
//
// Two things are NOT touched, and both are limits on what the caller's
// evidence covers rather than concessions:
//
//   - A file not named in files keeps its state entirely. The startup sweep
//     omits a file it never resumed — one whose name was never resolved, one
//     it did not reach before a storage fault, and every file of a job past
//     downloading — and an omission is silence, not a finding of absence.
//     This is why the file indices are carried separately from the runs: a
//     file whose runs were all discarded contributes NO run, and would be
//     indistinguishable from a file the sweep never looked at.
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
func (q *Queue) ReplaceFromRuns(jobID string, files []int32, runs []durability.Run) error {
	if len(files) == 0 {
		return nil
	}
	q.mu.Lock()
	// A PAUSED job is not resident, and it is exactly the job that needs this.
	//
	// The startup sweep used to skip it, so #362 survived in that branch: its
	// disproven Done bits were never corrected, the next checkpoint re-recorded
	// them, and the file finalized over a hole. It also defeated stallLost's own
	// "restart gonzbd to resume this job" instruction, since Stall leaves the
	// job Paused.
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
			return fmt.Errorf("queue: ReplaceFromRuns %s: %w", jobID, err)
		}
		if hErr := q.hydrateJobLocked(j, jobID); hErr != nil { //lockio: reads the manifest for a non-resident job; startup only, see the comment above
			q.mu.Unlock()
			return fmt.Errorf("queue: ReplaceFromRuns %s: %w", jobID, hErr)
		}
		if j.manifest == nil {
			q.mu.Unlock()
			return fmt.Errorf("queue: ReplaceFromRuns %s: %w", jobID, err)
		}
		job, evictAfter = j, j
	}
	// abandonLocked undoes this call's hydration and releases the lock, in
	// that order, for the paths that give up before the write-back.
	//
	// It exists because the eviction at the bottom is the ONLY one, and the
	// two validation returns below jumped over it — a job this call hydrated
	// for the duration stayed resident with its manifest attached, against a
	// residency budget docs/queue-lifecycle.md exists to bound. A malformed
	// run or an out-of-range file index is a bad argument, not a reason to
	// promote a job nobody asked to promote.
	abandonLocked := func() {
		if evictAfter != nil {
			q.evictJobLocked(evictAfter)
		}
		q.mu.Unlock()
	}

	m := job.manifest
	// Covered is built over the WHOLE job before any file is walked, because a
	// run names articles globally and the loop below asks the question per
	// article rather than per run. Runs for a file the sweep did not name are
	// simply never consulted — the file loop is what bounds the authority.
	covered := make([]bool, m.NumArticles())
	for _, r := range runs {
		lo, hi, cErr := runsCoverage(m, r)
		if cErr != nil {
			abandonLocked()
			return fmt.Errorf("queue: ReplaceFromRuns %s: %w", jobID, cErr)
		}
		for i := lo; i <= hi; i++ {
			covered[i] = true
		}
	}
	var cleared int
	for _, fi := range files {
		f := int(fi)
		if f < 0 || f >= m.NumFiles() {
			abandonLocked()
			return fmt.Errorf("queue: ReplaceFromRuns %s: file index %d out of range (%d files)",
				jobID, f, m.NumFiles())
		}
		lo, hi := m.FileRange(f)
		fileCleared := 0
		for i := lo; i < hi; i++ {
			if covered[i] {
				job.progress.markDone(m, i)
				continue
			}
			if job.progress.markNotDone(i) {
				fileCleared++
			}
		}
		if fileCleared > 0 {
			fp := &job.progress.files[f]
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
	// re-derives the article state from the durability record unconditionally
	// — PromoteNext, hydrateSnapshot, SetStatus all reach
	// Store.RestoreJobProgress — and the row that carries the CORRECTION is
	// job_files itself: Complete and assembled_crc32 are cleared in memory
	// here and nowhere else. Until that row is rewritten, any eviction and
	// re-promotion brings the file's Complete flag and AssembledCRC32 back,
	// over articles this sweep has just returned to Outstanding. The first
	// periodic save can be a whole checkpoint interval away.
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
		persistErr = q.store.Update(context.Background(), job) //lockio: persists the cleared Complete/CRC before any re-hydration can re-read the stale row
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
		return fmt.Errorf("queue: ReplaceFromRuns %s: persist cleared articles: %w", jobID, persistErr)
	}
	return nil
}

// runsCoverage returns the inclusive GLOBAL article range one run accounts
// for, after checking it against the file the run claims.
//
// It exists because both seeding entry points need the same check and it is
// easy to get wrong in a way no bounds check downstream catches. A run's
// FirstArtIdx and LastArtIdx are already global — that is exactly what
// replaced the file-local ordinal conversion the deleted durable bitmap
// needed — so there is no arithmetic here, only the verification that the pair
// really lies inside the file the row names.
//
// A run that does not is refused LOUDLY rather than clamped or skipped (A2,
// R28). It means the manifest was rebuilt to a different shape under rows
// keyed on the old numbering, and marking whatever articles happen to sit at
// those indices done would skip real downloads silently. The retry path
// already drops the rows for that case before requeuing; anything reaching
// here has escaped it.
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
