package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// TestClearAllEmitted_RestoresFailedArticleBytes verifies that ClearAllEmitted
// decrements FailedBytes and restores RemainingBytes for failed articles.
// This is the byte-accounting invariant: retried articles must not be
// permanently counted as lost data.
func TestClearAllEmitted_RestoresFailedArticleBytes(t *testing.T) {
	t.Parallel()

	q := New()
	if err := q.Add(makeTestJob("j1", 1, 3)); err != nil {
		t.Fatal(err)
	}

	// Mark one article done (should not be touched by ClearAllEmitted).
	ackDone(t, q, "j1", artID(0, 0))
	// Mark one article failed (should be retried by ClearAllEmitted).
	ackFailed(t, q, "j1", artID(0, 1))

	snap := q.SnapshotJob("j1")
	failedBefore := snap.Progress().FailedBytes()
	remainingBefore := snap.Progress().RemainingBytes()
	if failedBefore == 0 {
		t.Fatal("precondition: FailedBytes must be > 0 after MarkArticleFailed")
	}

	q.ClearAllEmitted(nil)

	snap = q.SnapshotJob("j1")
	if snap.Progress().FailedBytes() != 0 {
		t.Errorf("FailedBytes = %d after ClearAllEmitted; want 0", snap.Progress().FailedBytes())
	}
	if snap.Progress().RemainingBytes() != remainingBefore+failedBefore {
		t.Errorf("RemainingBytes = %d; want %d (restored from FailedBytes)",
			snap.Progress().RemainingBytes(), remainingBefore+failedBefore)
	}
}

// TestClearAllEmitted_StatusDownloadingPreserved verifies that jobs in
// StatusDownloading remain in StatusDownloading after ClearAllEmitted, preserving
// ActiveSet residency for the new downloader.
func TestClearAllEmitted_StatusDownloadingPreserved(t *testing.T) {
	t.Parallel()

	q := New()
	if err := q.Add(makeTestJob("j1", 1, 2)); err != nil {
		t.Fatal(err)
	}
	// Drive the job into Downloading state.
	if err := q.SetStatus("j1", constants.StatusDownloading); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	snap := q.SnapshotJob("j1")
	if snap.Status != constants.StatusDownloading {
		t.Fatalf("precondition: status = %v, want StatusDownloading", snap.Status)
	}

	q.ClearAllEmitted(nil)

	snap = q.SnapshotJob("j1")
	if snap.Status != constants.StatusDownloading {
		t.Errorf("status = %v after ClearAllEmitted; want StatusDownloading", snap.Status)
	}
}

// TestClearAllEmitted_CompletedArticlesUntouched verifies that successfully
// downloaded articles (Done=true, Failed=false) survive ClearAllEmitted
// intact. Re-downloading completed articles would waste bandwidth and
// produce duplicate data.
func TestClearAllEmitted_CompletedArticlesUntouched(t *testing.T) {
	t.Parallel()

	q := New()
	if err := q.Add(makeTestJob("j1", 1, 3)); err != nil {
		t.Fatal(err)
	}

	// Complete all three articles.
	for ai := range 3 {
		ackDone(t, q, "j1", artID(0, ai))
	}

	snap := q.SnapshotJob("j1")
	remainingBefore := snap.Progress().RemainingBytes()

	q.ClearAllEmitted(nil)

	snap = q.SnapshotJob("j1")
	if snap.Progress().RemainingBytes() != remainingBefore {
		t.Errorf("RemainingBytes changed from %d to %d; completed articles must not be affected",
			remainingBefore, snap.Progress().RemainingBytes())
	}
}

// TestClearAllEmitted_ResetsEmittedSoDispatcherRetries verifies the primary
// function of ClearAllEmitted: emitted (in-flight) articles are cleared so
// the dispatcher can re-dispatch them on the next pass.
func TestClearAllEmitted_ResetsEmittedSoDispatcherRetries(t *testing.T) {
	t.Parallel()

	q := New()
	if err := q.Add(makeTestJob("j1", 1, 2)); err != nil {
		t.Fatal(err)
	}

	// Simulate two in-flight articles.
	_ = q.MarkArticleEmittedByIdx("j1", artIdxFor(t, q, "j1", artID(0, 0)))
	_ = q.MarkArticleEmittedByIdx("j1", artIdxFor(t, q, "j1", artID(0, 1)))

	var countBefore int
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		countBefore++
		return true
	})
	// Emitted articles are skipped by ForEachUnfinishedArticle.
	if countBefore != 0 {
		t.Fatalf("before ClearAllEmitted: ForEach saw %d articles, want 0 (both emitted)", countBefore)
	}

	q.ClearAllEmitted(nil)

	var countAfter int
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		countAfter++
		return true
	})
	if countAfter != 2 {
		t.Errorf("after ClearAllEmitted: ForEach saw %d articles, want 2 (emitted cleared)", countAfter)
	}
}

// TestForEachUnfinishedArticle_EarlyExitStops verifies that returning false
// from the callback stops iteration. This is the dispatcher's early-exit
// path used when the work channel is full.
func TestForEachUnfinishedArticle_EarlyExitStops(t *testing.T) {
	t.Parallel()

	q := New()
	if err := q.Add(makeTestJob("j1", 1, 5)); err != nil {
		t.Fatal(err)
	}

	var count int
	q.ForEachUnfinishedArticle(func(UnfinishedArticle) bool {
		count++
		return count < 2 // stop after seeing 2
	})
	if count != 2 {
		t.Errorf("count = %d; want 2 (early exit should have stopped at 2)", count)
	}
}

// TestClearAllEmitted_WithholdsTheEmittedClearForASkippedJob is #417.
//
// A job whose reload checkpoint could not make its written articles durable
// keeps its Emitted bits, so they are not offered for re-fetch against a server
// set the user has just changed. A job the checkpoint did protect is cleared as
// before — the skip must be per job, or every settings change would stall the
// articles it was supposed to re-dispatch.
func TestClearAllEmitted_WithholdsTheEmittedClearForASkippedJob(t *testing.T) {
	t.Parallel()

	q := New()
	if err := q.Add(makeTestJob("protected", 1, 2)); err != nil {
		t.Fatal(err)
	}
	if err := q.Add(makeTestJob("unprotected", 1, 2)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"protected", "unprotected"} {
		for ai := range 2 {
			if err := q.MarkArticleEmittedByIdx(id, artIdxFor(t, q, id, artID(0, ai))); err != nil {
				t.Fatal(err)
			}
		}
	}

	q.ClearAllEmitted(map[string]struct{}{"unprotected": {}})

	seen := map[string]int{}
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		seen[a.JobID]++
		return true
	})

	if seen["protected"] != 2 {
		t.Errorf("the protected job offers %d articles, want 2: its checkpoint succeeded, "+
			"so withholding its bits stalls work for no reason", seen["protected"])
	}
	if seen["unprotected"] != 0 {
		t.Errorf("the skipped job offers %d articles, want 0: its written-but-unacked "+
			"bytes are on disk, and re-fetching them against a changed server set is "+
			"what marks them permanently failed", seen["unprotected"])
	}
}

// TestClearAllEmitted_StillUnfailsASkippedJobsArticles pins the disjointness
// the narrow skip rests on.
//
// The skip withholds the Emitted clear and NOTHING else. markFailed clears
// `emitted` as it sets `failed`, and the un-fail arm is gated on `failed`, so
// the two act on disjoint articles. Skipping the un-fail as well would leave an
// article the old downloader's teardown failed — ErrNoServersLeft is terminal —
// failed forever: markNotDone refuses a permanently failed article, and a
// restart re-applies the persisted row. That would trade one permanent strand
// for another, on the same inflated failedBytes this change exists to prevent.
func TestClearAllEmitted_StillUnfailsASkippedJobsArticles(t *testing.T) {
	t.Parallel()

	q := New()
	if err := q.Add(makeTestJob("j1", 1, 3)); err != nil {
		t.Fatal(err)
	}
	// One article in flight, one failed during the old downloader's teardown.
	if err := q.MarkArticleEmittedByIdx("j1", artIdxFor(t, q, "j1", artID(0, 0))); err != nil {
		t.Fatal(err)
	}
	ackFailed(t, q, "j1", artID(0, 1))

	failedBefore := q.SnapshotJob("j1").Progress().FailedBytes()
	if failedBefore == 0 {
		t.Fatal("fixture: the teardown failure recorded no bytes")
	}

	q.ClearAllEmitted(map[string]struct{}{"j1": {}})

	if got := q.SnapshotJob("j1").Progress().FailedBytes(); got != 0 {
		t.Errorf("FailedBytes = %d after a skipped job's reload, want 0. The skip must "+
			"withhold the emitted clear only; leaving the article failed strands it "+
			"permanently and keeps the figure that aborts healthy jobs", got)
	}

	// And the emitted article is still withheld, so the two halves really did
	// take different paths in the same call.
	var offered int
	q.ForEachUnfinishedArticle(func(UnfinishedArticle) bool {
		offered++
		return true
	})
	if offered == 0 {
		t.Error("the un-failed article is not offered for re-dispatch, so the skip " +
			"suppressed more than the emitted clear")
	}
}
