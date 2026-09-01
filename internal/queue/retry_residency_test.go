package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"

	"github.com/hobeone/gonzbd/internal/fsutil"
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
// test still calls q.Save(dir) before evicting because that matches
// production: a real daemon periodically flushes progress to SQLite while a
// job downloads.
//
// What the Save no longer supplies is the FAILED bit. Article resolution is
// derived from durable_runs and failed_articles, and AckPermanentFailure
// writes its failed_articles row at ack time rather than waiting for a flush
// (see Queue.AckPermanentFailure and the failedPersistMu doc). So the stale
// state this test needs is on disk before the Save runs. Measured, not
// assumed: neutering the Save below leaves this test green.
func TestRetry_StoreBackedNonResident_PreservesSuccessAndRetriesFailed(t *testing.T) {
	t.Parallel()
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "retry-job", 1, 2) // 1 file, 2 articles
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !manifestResident(job) {
		t.Fatal("fixture guard: job should be resident (promoted to Downloading) immediately after Add")
	}

	okID := articleID(0, 0)
	failID := articleID(0, 1)

	ackDone(t, q, job.ID, okID)
	ackFailed(t, q, job.ID, failID)

	// Flush to SQLite -- the step the previous, ineffective fix attempt's test
	// skipped, kept because it is what production does. It is no longer what
	// puts the failed article on disk; see the note on this function.
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
	if manifestResident(job) {
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
	if !manifestResident(job) || job.Progress() == nil {
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

	// Reversion check: reverting the persist-before-PromoteNext work Retry does
	// makes this test fail, because PromoteNext's own RestoreJobProgress call
	// re-derives resolution from the stale records — article 1's failed_articles
	// row, written back at ack time, which resolves as done-because-failed — and
	// re-marks article 1 done, so it is never offered again. That is why Retry
	// clears the job's failed_articles rows and persists the reset before
	// promoting.
}

// TestRetry_HydrationFailureLeavesStatusUnchanged pins the #264-style
// fail-closed contract for Retry's own hydration step: if the manifest
// cannot be read from disk, Retry must return an error and must not mutate
// job.Status, matching SetStatus/SetStatusIf's existing behavior.
func TestRetry_HydrationFailureLeavesStatusUnchanged(t *testing.T) {
	t.Parallel()
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "retry-hydrate-fail", 1, 1)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.SetStatus(job.ID, constants.StatusFailed); err != nil {
		t.Fatalf("SetStatus(Failed): %v", err)
	}
	if manifestResident(job) {
		t.Fatal("fixture guard: job should be non-resident after StatusFailed")
	}

	// Corrupt the manifest file so hydration fails.
	manifestPath := dir + "/manifests/" + job.ID + ".json.gz"
	if err := fsutil.WriteGzAtomicBytes(manifestPath, []byte("not valid gzip json")); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}

	if err := q.Retry(job.ID); err == nil {
		t.Fatal("expected Retry to return an error when hydration fails")
	}

	if job.Status != constants.StatusFailed {
		t.Errorf("job.Status = %s, want unchanged StatusFailed after failed hydration", job.Status)
	}
	if manifestResident(job) {
		t.Error("job should remain non-resident after failed hydration")
	}
}

// TestResolveArticles_KeepsAFailedArticleDistinctFromAPlainDoneOne is the
// direct unit-level pin on the derived resolution, and it is #260's root
// defect stated against the mechanism that replaced the encoder: a failed
// article must come back done AND failed, never as a plain successful one.
//
// It is also where failed-implies-done is fixed. Both consumers read Failed
// only inside the Done branch — markFailed early-returns once done is set, and
// newJobProgressSized nests the two the same way — so a failed article that
// did not also read as done would come back Pending and be re-fetched on every
// restart, forever.
//
// A failed article is deliberately NOT covered by any run in the fixture: it
// never decoded, so nothing was written for it and no fsync could have
// recorded it. The done bit it gets is resolveArticles', not a run's.
func TestResolveArticles_KeepsAFailedArticleDistinctFromAPlainDoneOne(t *testing.T) {
	t.Parallel()
	// Article 0 is covered by a run; article 1 permanently failed; articles
	// 2 and 3 are still outstanding.
	done, failed := resolveArticles([]artRange{{First: 0, Last: 0}}, []int32{1}, 4)

	for i, want := range []struct{ done, failed bool }{
		{true, false},  // 0: covered by a run
		{true, true},   // 1: failed, and done BECAUSE failed
		{false, false}, // 2: outstanding
		{false, false}, // 3: outstanding
	} {
		if done[i] != want.done || failed[i] != want.failed {
			t.Errorf("article %d resolved done=%v failed=%v, want done=%v failed=%v",
				i, done[i], failed[i], want.done, want.failed)
		}
	}
}

// TestResolveArticles_ClampsARunOutsideTheManifest pins the corrupt-row branch.
//
// A run naming an article the manifest does not have means the manifest was
// rebuilt to a different shape under rows keyed on the old numbering — the
// condition RetryHistoryJob decides and drops the rows for. Anything reaching
// here has escaped that, and it must not index out of bounds on the boot path.
func TestResolveArticles_ClampsARunOutsideTheManifest(t *testing.T) {
	t.Parallel()
	done, failed := resolveArticles([]artRange{{First: -3, Last: 99}}, []int32{-1, 42}, 2)

	if len(done) != 2 || len(failed) != 2 {
		t.Fatalf("resolved %d/%d articles, want 2/2", len(done), len(failed))
	}
	if !done[0] || !done[1] {
		t.Errorf("the in-range part of the run resolved done=%v,%v, want both true — "+
			"clamping must not discard the coverage it can interpret", done[0], done[1])
	}
	if failed[0] || failed[1] {
		t.Errorf("an out-of-range failed index marked an in-range article failed: %v", failed)
	}
}

// TestRetry_RestartRoundTrip_PreservesFailedSet exercises a full
// Save -> Loader.Load -> Retry cycle: the failed-article set must survive a
// process restart (not just an in-process eviction), since RestoreJobProgress
// and Loader.Load both derive it from the same failed_articles rows.
func TestRetry_RestartRoundTrip_PreservesFailedSet(t *testing.T) {
	t.Parallel()
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "restart-retry", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	okID := articleID(0, 0)
	failID := articleID(0, 1)
	ackDone(t, q, job.ID, okID)
	ackFailed(t, q, job.ID, failID)
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
	// job is now non-resident (manifest/progress nil), preserving the per-file
	// row written by the first Save above. The durability records are not
	// written by Save at all.
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save (post-failure): %v", err)
	}

	loaded, err := Load(dir, WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	loadedJob, ok := loaded.byID[job.ID]
	if !ok {
		t.Fatalf("job %s missing after Load", job.ID)
	}
	if manifestResident(loadedJob) {
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
