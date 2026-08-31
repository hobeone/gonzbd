package queue

import (
	"fmt"

	"github.com/hobeone/gonzbd/internal/durability"
)

// Manifest-tier operations on one job: everything that resolves a file index
// or an article index against a resident Manifest. They were Queue method
// bodies until B2.4a; their Queue methods are now lookup plus one of these
// plus queue bookkeeping.
//
// EVERY METHOD HERE MUST BE CALLED WITH Queue.mu HELD, and every one begins
// with j.resident(). The residency call is not defensive duplication of the
// caller's check — it is the gate itself in its Job-level form (#261), and
// TestManifestAccessIsGated fails any method in this file that dereferences
// j.manifest without it.
//
// Held for WRITING by every method here but one, because they mutate progress.
// The exception is countUnfinishedArticles, which only reads: its caller
// Queue.CountUnfinishedArticles holds q.mu for reading. Stated as an
// enumeration rather than as a universal because the universal was false as
// soon as the one query moved in with the mutations.
//
// # What these return
//
// Two methods do not return an error naming the job. undeferRecovery returns
// no error at all, reporting a bare changed bool — its two callers differ on
// whether an out-of-range index is a refusal or a no-op, so the rejecting one
// gets its error from checkFileIdxs instead and this one has nothing left to
// report. seedFromRuns names the job in its residency error but not in its
// coverage error, which comes from runsCoverage unwrapped; see below for what
// supplies the ID there. Every other method here names the job in every error
// it returns.
//
// Where an error IS returned, the Queue wrapper passes it through unchanged
// rather than re-wrapping. That is not a style choice: before B2.4a,
// Queue.residentJob built the residency error with the ID and the method body
// built the index error with the ID, so both already carried it. A wrapper
// that re-wrapped whatever came back would produce
//
//	queue: fileIdx 9 out of range for job j1: j1
//
// AckDurable and SeedFromRuns are the exceptions, and they were exceptions
// before this change too: both add their own "queue: <method> <id>: " prefix.
// For AckDurable the ID appears twice in the result, because every error it
// returns already carries it. For SeedFromRuns that holds of the residency
// error only — the coverage error comes from runsCoverage, which names the
// file and article range but not the job, so the wrapper's prefix supplies the
// single occurrence.
// TestManifestTierErrorShapes pins all of it, and passed against the unmoved
// code before this file existed.

// countUnfinishedArticles returns the number of articles in fileIdx that are
// not yet Done.
//
// Counts !Done rather than file.Pending, which tracks !Done && !Emitted. The
// difference matters on resume: an Emitted article is in flight and not yet
// durably committed, so it is still unfinished for the assembler's TotalParts.
func (j *Job) countUnfinishedArticles(fileIdx int) (int, error) {
	if err := j.resident(); err != nil {
		return 0, fmt.Errorf("%w: %s", err, j.ID)
	}
	if fileIdx < 0 || fileIdx >= j.manifest.NumFiles() {
		return 0, fmt.Errorf("queue: fileIdx %d out of range for job %s", fileIdx, j.ID)
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

// markArticleEmittedByIdx flags an article as having a result in flight.
//
// Non-residency and a genuine bounds violation are reported separately:
// collapsing them misdiagnoses a de-hydrated job as a caller bug.
func (j *Job) markArticleEmittedByIdx(artIdx int32) error {
	if err := j.resident(); err != nil {
		return fmt.Errorf("%w: %s", err, j.ID)
	}
	if int(artIdx) < 0 || int(artIdx) >= j.manifest.NumArticles() {
		return fmt.Errorf("queue: artIdx %d out of range for job %s", artIdx, j.ID)
	}
	j.progress.markEmitted(j.manifest, int(artIdx))
	return nil
}

// clearArticleEmittedByIdx resets the transient Emitted flag, returning the
// article to the dispatch pool.
//
// clearEmitted only restores it to pending if it is not already done. An
// article can be Emitted and Done at once when AckDurable ran first — a late
// assembler flush after a downloader reload — and is finished then.
func (j *Job) clearArticleEmittedByIdx(artIdx int32) error {
	if err := j.resident(); err != nil {
		return fmt.Errorf("%w: %s", err, j.ID)
	}
	if int(artIdx) < 0 || int(artIdx) >= j.manifest.NumArticles() {
		return fmt.Errorf("queue: artIdx %d out of range for job %s", artIdx, j.ID)
	}
	j.progress.clearEmitted(j.manifest, int(artIdx))
	return nil
}

// markFileComplete marks the file at fileIdx as fully assembled.
func (j *Job) markFileComplete(fileIdx int) error {
	if err := j.resident(); err != nil {
		return fmt.Errorf("%w: %s", err, j.ID)
	}
	if fileIdx < 0 || fileIdx >= j.manifest.NumFiles() {
		return fmt.Errorf("queue: fileIdx %d out of range for job %s", fileIdx, j.ID)
	}
	j.progress.files[fileIdx].Complete = true
	return nil
}

// setFileFilename stores the resolved final filename on a file.
func (j *Job) setFileFilename(fileIdx int, filename string) error {
	if err := j.resident(); err != nil {
		return fmt.Errorf("%w: %s", err, j.ID)
	}
	if fileIdx < 0 || fileIdx >= j.manifest.NumFiles() {
		return fmt.Errorf("queue: fileIdx %d out of range for job %s", fileIdx, j.ID)
	}
	j.progress.files[fileIdx].Filename = filename
	return nil
}

// setFileCRC32FromRuns stores the assembled CRC32 on a file, but only if the
// runs are evidence for it, and reports whether it stored anything.
//
// It takes the RUNS rather than a uint32 deliberately: the CRC's meaning is
// entirely a property of the record it came from, so a setter accepting a
// bare value has no way to refuse a wrong one. A run set that is not exactly
// one run covering the whole file from offset zero proves nothing about the
// assembled bytes, so it records nothing and reports no change — which is not
// an error.
func (j *Job) setFileCRC32FromRuns(fileIdx int, runs []durability.Run) (bool, error) {
	if err := j.resident(); err != nil {
		return false, fmt.Errorf("%w: %s", err, j.ID)
	}
	if fileIdx < 0 || fileIdx >= j.manifest.NumFiles() {
		return false, fmt.Errorf("queue: fileIdx %d out of range for job %s", fileIdx, j.ID)
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

// checkFileIdxs reports the first out-of-range index in fileIdxs.
//
// Separate from undeferRecovery because the two callers want opposite
// answers: UndeferRecoveryVolumes rejects the whole request if any index is
// bad, while AckPermanentFailure passes DeferredRecoveryIndices and needs the
// apply step to ignore anything stale rather than fail.
func (j *Job) checkFileIdxs(fileIdxs []int) error {
	if err := j.resident(); err != nil {
		return fmt.Errorf("%w: %s", err, j.ID)
	}
	for _, fi := range fileIdxs {
		if fi < 0 || fi >= j.manifest.NumFiles() {
			return fmt.Errorf("queue: fileIdx %d out of range for job %s", fi, j.ID)
		}
	}
	return nil
}

// undeferRecovery clears the on-demand hold on the given recovery volumes,
// re-activating their articles for dispatch, and reports whether anything
// changed.
//
// RemainingBytes needs no fixup of its own: it derives from the fetch policy
// on every read, so clearing the hold is what makes the file's bytes start
// counting as remaining — see derivedRemainingBytes. Indices that are out of
// range or not deferred are ignored; see checkFileIdxs for why that is not
// laxity.
//
// Was Queue.undeferRecoveryLocked until B2.4a. The q.dirty and q.notifyLocked
// it used to perform moved to its two callers, which is why it returns
// changed.
func (j *Job) undeferRecovery(fileIdxs []int) bool {
	if err := j.resident(); err != nil {
		return false
	}
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

// ackDurable applies a durability proof's articles, returning how many named
// an article this job does not have, and how many it does have.
//
// The apply is idempotent because markDone is (R12). At-least-once delivery is
// the contract — SyncTarget.Drain may re-report an article a previous Drain
// returned — so a replayed proof must not double-count bytes; markDone early-
// returns on an already-done article before touching BytesDownloaded.
//
// The two counts go back to the caller rather than being logged here, because
// Queue.AckDurable logs them AFTER releasing the lock and this runs under it.
func (j *Job) ackDurable(arts []int32) (invalid, nArt int, err error) {
	if err := j.resident(); err != nil {
		return 0, 0, fmt.Errorf("%w: %s", err, j.ID)
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

// seedFromRuns marks every article covered by runs as done.
//
// Every run is checked before any article is marked, which is the same
// validate-then-apply shape ReplaceFromRuns uses to build its covered set.
// The interleaved form this replaced applied each run as it cleared, which
// leaked exactly the corruption runsCoverage exists to detect: a run it
// refuses means the manifest was rebuilt under rows keyed on the old
// numbering, and that is a statement about the whole batch, not about the one
// row. The rows ordered before it are keyed on the same stale numbering and
// passed only because their indices happened to land inside a valid range, so
// applying them marks arbitrary articles done. markDone is one-way on this
// path (R9) and decrements pendingArticles, which is what
// ForEachUnfinishedArticle skips a job on, so those articles are never
// enumerated for dispatch again — the silent skipped download runsCoverage
// refuses loudly to allow. It also restores this method's contract to what
// its caller already documents: seedFromCommittedRuns logs the error and
// carries on because "a failure here costs a re-fetch and nothing else",
// which only holds if a failure applies nothing.
func (j *Job) seedFromRuns(runs []durability.Run) error {
	if err := j.resident(); err != nil {
		return fmt.Errorf("%w: %s", err, j.ID)
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
