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
	if err := q.MarkArticleDone("j1", artID(0, 0)); err != nil {
		t.Fatal(err)
	}
	// Mark one article failed (should be retried by ClearAllEmitted).
	if _, err := q.MarkArticleFailed("j1", artID(0, 1)); err != nil {
		t.Fatal(err)
	}

	snap := q.SnapshotJob("j1")
	failedBefore := snap.Progress().FailedBytes()
	remainingBefore := snap.Progress().RemainingBytes()
	if failedBefore == 0 {
		t.Fatal("precondition: FailedBytes must be > 0 after MarkArticleFailed")
	}

	q.ClearAllEmitted()

	snap = q.SnapshotJob("j1")
	if snap.Progress().FailedBytes() != 0 {
		t.Errorf("FailedBytes = %d after ClearAllEmitted; want 0", snap.Progress().FailedBytes())
	}
	if snap.Progress().RemainingBytes() != remainingBefore+failedBefore {
		t.Errorf("RemainingBytes = %d; want %d (restored from FailedBytes)",
			snap.Progress().RemainingBytes(), remainingBefore+failedBefore)
	}
}

// TestClearAllEmitted_StatusDownloadingBecomesQueued verifies that jobs in
// StatusDownloading are reset to StatusQueued. The old downloader is gone;
// the new downloader's first dispatch pass transitions back to Downloading.
func TestClearAllEmitted_StatusDownloadingBecomesQueued(t *testing.T) {
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

	q.ClearAllEmitted()

	snap = q.SnapshotJob("j1")
	if snap.Status != constants.StatusQueued {
		t.Errorf("status = %v after ClearAllEmitted; want StatusQueued", snap.Status)
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
		if err := q.MarkArticleDone("j1", artID(0, ai)); err != nil {
			t.Fatalf("MarkArticleDone art %d: %v", ai, err)
		}
	}

	snap := q.SnapshotJob("j1")
	remainingBefore := snap.Progress().RemainingBytes()

	q.ClearAllEmitted()

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
	_ = q.MarkArticleEmitted("j1", artID(0, 0))
	_ = q.MarkArticleEmitted("j1", artID(0, 1))

	var countBefore int
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		countBefore++
		return true
	})
	// Emitted articles are skipped by ForEachUnfinishedArticle.
	if countBefore != 0 {
		t.Fatalf("before ClearAllEmitted: ForEach saw %d articles, want 0 (both emitted)", countBefore)
	}

	q.ClearAllEmitted()

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
