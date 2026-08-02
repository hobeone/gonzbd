package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestRetry_StoreBackedNonResident_PreservesSuccessAndRetriesFailed pins
// issue #260: Queue.Retry on a store-backed queue must actually re-dispatch
// a job's failed articles while keeping its successful ones, even when the
// job is non-resident at the moment Retry is called (the ordinary state for
// a Failed job, since evictJobLocked drops manifest/progress on the
// transition into StatusFailed).
//
// The previous attempt's test never flushed to the store, so the
// persistence round trip -- the actual defect -- was never exercised. This
// test explicitly calls q.Save(dir) before evicting, matching production:
// a real daemon periodically flushes progress to SQLite while a job
// downloads, so job_files.articles_done holds real state by the time a job
// fails and is evicted.
func TestRetry_StoreBackedNonResident_PreservesSuccessAndRetriesFailed(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "retry-job", 1, 2) // 1 file, 2 articles
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if job.Manifest() == nil {
		t.Fatal("fixture guard: job should be resident (promoted to Downloading) immediately after Add")
	}

	okID := articleID(0, 0)
	failID := articleID(0, 1)

	if err := q.MarkArticlesDone(job.ID, []string{okID}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if _, err := q.MarkArticlesFailed(job.ID, []string{failID}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}

	// Flush to SQLite -- this is the step the previous, ineffective fix
	// attempt's test skipped. Without it job_files.articles_done never
	// holds the failed bit and the bug is not exercised.
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := q.SetStatus(job.ID, constants.StatusFailed); err != nil {
		t.Fatalf("SetStatus(Failed): %v", err)
	}

	// Fixture guard: the job must actually be non-resident here, or this
	// test silently stops exercising the bug (issue #260's core scenario).
	// Manifest is the residency signal; progress is never released
	// (docs/queue-lifecycle.md), so it is checked separately below rather
	// than folded into this guard.
	if job.Manifest() != nil {
		t.Fatal("fixture guard: job should be non-resident after transitioning to StatusFailed")
	}
	if job.Progress() == nil {
		t.Fatal("fixture guard: progress must never be nil")
	}

	if err := q.Retry(job.ID); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	// Retry calls PromoteNext internally; assert on the END state, after
	// promotion has run -- this is what catches the "second trap"
	// (PromoteNext's unconditional RestoreJobProgress undoing the reset).
	if job.Manifest() == nil || job.Progress() == nil {
		t.Fatal("expected job to be resident again after Retry promotes it")
	}
	if job.Status != constants.StatusDownloading {
		t.Fatalf("expected job promoted back to Downloading, got %s", job.Status)
	}

	var offered []string
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		if a.JobID == job.ID {
			offered = append(offered, a.MessageID)
		}
		return true
	})
	if len(offered) != 1 || offered[0] != failID {
		t.Fatalf("ForEachUnfinishedArticle offered %v, want exactly [%s] (the failed article re-dispatchable, the successful one not re-offered)", offered, failID)
	}

	p := job.Progress()
	if p.ArticlesFailed() != 0 {
		t.Errorf("ArticlesFailed = %d, want 0 (failed article was reset to pending)", p.ArticlesFailed())
	}
	if p.ArticlesResolved() != 1 {
		t.Errorf("ArticlesResolved = %d, want 1 (only the successful article remains resolved)", p.ArticlesResolved())
	}
	if p.FailedBytes() != 0 {
		t.Errorf("FailedBytes = %d, want 0", p.FailedBytes())
	}
	// remainingBytes: total 200_000 minus the 100_000 kept-successful article.
	if got, want := p.RemainingBytes(), int64(100_000); got != want {
		t.Errorf("RemainingBytes = %d, want %d", got, want)
	}
	if p.FileBytesDownloaded(0) != 100_000 {
		t.Errorf("FileBytesDownloaded(0) = %d, want 100_000 (only the successful article's bytes)", p.FileBytesDownloaded(0))
	}
	if !p.ArticleDone(0) || p.ArticleFailed(0) {
		t.Error("article 0 (success) should remain done and not failed")
	}
	if p.ArticleDone(1) || p.ArticleFailed(1) {
		t.Error("article 1 (was failed) should be reset to pending: not done, not failed")
	}

	// Reversion check: reverting the q.store.Update call added to Retry (the
	// persist-before-PromoteNext line) makes this test fail, because
	// PromoteNext's own RestoreJobProgress call re-reads the stale SQLite
	// row (still encoding article 1 as done, from the pre-Retry Save) and
	// re-marks article 1 done, so it is never offered again.
}

// TestRetry_HydrationFailureLeavesStatusUnchanged pins the #264-style
// fail-closed contract for Retry's own hydration step: if the manifest
// cannot be read from disk, Retry must return an error and must not mutate
// job.Status, matching SetStatus/SetStatusIf's existing behavior.
func TestRetry_HydrationFailureLeavesStatusUnchanged(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "retry-hydrate-fail", 1, 1)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.SetStatus(job.ID, constants.StatusFailed); err != nil {
		t.Fatalf("SetStatus(Failed): %v", err)
	}
	if job.Manifest() != nil {
		t.Fatal("fixture guard: job should be non-resident after StatusFailed")
	}

	// Corrupt the manifest file so hydration fails.
	manifestPath := dir + "/manifests/" + job.ID + ".json.gz"
	if err := writeGzJSONRaw(manifestPath, []byte("not valid gzip json")); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}

	if err := q.Retry(job.ID); err == nil {
		t.Fatal("expected Retry to return an error when hydration fails")
	}

	if job.Status != constants.StatusFailed {
		t.Errorf("job.Status = %s, want unchanged StatusFailed after failed hydration", job.Status)
	}
	if job.Manifest() != nil {
		t.Error("job should remain non-resident after failed hydration")
	}
}

// TestSQLiteStore_EncodeDecodeArticlesDone_RoundTripsFailedBit is the direct
// unit-level pin for the encode/decode widening: a failed article must
// round-trip distinctly from a plain done article through the SQLite
// bitmap encoding, which is the root defect in issue #260 (markFailed's
// done+failed bits collapsed to a plain "done" bit by the old encoder).
func TestSQLiteStore_EncodeDecodeArticlesDone_RoundTripsFailedBit(t *testing.T) {
	job := makeMultiFileJob(t, "roundtrip-failed", 1, 2)
	job.Progress().markDone(job.Manifest(), 0)
	if !job.progress.markFailed(job.manifest, 1) {
		t.Fatal("markFailed should report a first-time transition")
	}

	encoded := encodeArticlesDone(job, 0)
	if encoded == "" {
		t.Fatal("expected non-empty encoding")
	}

	job2 := makeMultiFileJob(t, "roundtrip-failed-2", 1, 2)
	decodeTestStore(t).decodeArticlesDone(encoded, job2, 0)

	if !job2.Progress().ArticleDone(0) || job2.Progress().ArticleFailed(0) {
		t.Error("article 0 should decode as done, not failed")
	}
	if !job2.Progress().ArticleDone(1) || !job2.Progress().ArticleFailed(1) {
		t.Error("article 1 should decode as done AND failed -- this is the bit the old encoder dropped")
	}
}

// A bitmap whose length does not match the file's article count restores
// nothing.
//
// The decoder used to accept a short buffer as a legacy done-only bitmap and
// read whatever bits fit, which meant a truncated or foreign value silently
// restored a partial, wrong done-set — bytes accounted against articles that
// were never fetched. With only one encoding left, the sizes are fixed by
// the article count, so a mismatch means the value did not come from
// encodeArticlesDone for this file and no prefix of it can be trusted.
func TestSQLiteStore_DecodeArticlesDone_WrongLengthRestoresNothing(t *testing.T) {
	source := makeMultiFileJob(t, "wrong-length-src", 1, 2)
	source.Progress().markDone(source.Manifest(), 0)
	source.Progress().markDone(source.Manifest(), 1)
	full := encodeArticlesDone(source, 0)
	if full == "" {
		t.Fatal("fixture guard: empty encoding")
	}

	// Halve it: the exact shape the old legacy branch would have accepted.
	half := full[:len(full)/2]

	job := makeMultiFileJob(t, "wrong-length", 1, 2)
	decodeTestStore(t).decodeArticlesDone(half, job, 0)

	if job.Progress().ArticleDone(0) || job.Progress().ArticleDone(1) {
		t.Errorf("a half-length bitmap restored article state (done: %v, %v); a length mismatch means the value is not this file's, so no prefix of it is trustworthy",
			job.Progress().ArticleDone(0), job.Progress().ArticleDone(1))
	}

	// The full-length value still decodes, so the guard is not simply
	// rejecting everything.
	job2 := makeMultiFileJob(t, "wrong-length-ok", 1, 2)
	decodeTestStore(t).decodeArticlesDone(full, job2, 0)
	if !job2.Progress().ArticleDone(0) || !job2.Progress().ArticleDone(1) {
		t.Error("full-length bitmap failed to decode; the length guard is too strict")
	}
}

// TestRetry_RestartRoundTrip_PreservesFailedSet exercises a full
// Save -> Loader.Load -> Retry cycle: the failed-article set must survive a
// process restart (not just an in-process eviction), since RestoreJobProgress
// and Loader.Load share the same decodeArticlesDone path.
func TestRetry_RestartRoundTrip_PreservesFailedSet(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "restart-retry", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	okID := articleID(0, 0)
	failID := articleID(0, 1)
	if err := q.MarkArticlesDone(job.ID, []string{okID}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if _, err := q.MarkArticlesFailed(job.ID, []string{failID}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	// Flush while still resident, matching production: a live daemon
	// periodically persists progress while a job downloads, so by the time
	// SetStatus(Failed) evicts manifest/progress the SQLite row already
	// reflects the failure. Saving after eviction would flush a job with
	// manifest==nil/progress==nil, which updateTx skips entirely, and the
	// on-disk row would incorrectly still show the job's pre-failure state.
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := q.SetStatus(job.ID, constants.StatusFailed); err != nil {
		t.Fatalf("SetStatus(Failed): %v", err)
	}
	// A second flush persists the Failed status transition itself (the jobs
	// table's status column); job_files is untouched by this call since the
	// job is now non-resident (manifest/progress nil), preserving the
	// articles_done row written by the first Save above.
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save (post-failure): %v", err)
	}

	loaded, err := (&Loader{}).Load(dir, WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	loadedJob, ok := loaded.byID[job.ID]
	if !ok {
		t.Fatalf("job %s missing after Load", job.ID)
	}
	if loadedJob.Manifest() != nil {
		t.Fatal("fixture guard: reloaded Failed job should be non-resident")
	}

	if err := loaded.Retry(job.ID); err != nil {
		t.Fatalf("Retry after restart: %v", err)
	}

	var offered []string
	loaded.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		if a.JobID == job.ID {
			offered = append(offered, a.MessageID)
		}
		return true
	})
	if len(offered) != 1 || offered[0] != failID {
		t.Fatalf("after restart+Retry, ForEachUnfinishedArticle offered %v, want exactly [%s]", offered, failID)
	}
}
