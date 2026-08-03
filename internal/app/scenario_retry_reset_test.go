package app_test

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestRetry_ResetsDownloadStats verifies that RetryHistoryJob clears the
// transient download bookkeeping (DownloadStarted, ServerStats) so the
// retried attempt does not inherit stats from the previous attempt.
// Without the reset, OnJobDone's duration math (now - DownloadStarted)
// yields a huge bogus value and the per-server byte counts are doubled
// the next time the job finishes.
//
// Regression guard for B.3.
func TestRetry_ResetsDownloadStats(t *testing.T) {
	h := newScenarioHarness(t)
	h.Start()

	// Pause downloads so the requeued job sits at Queued long enough to be
	// inspected without racing the downloader/post-processor pipeline.
	h.app.PauseDownloads()
	t.Cleanup(h.app.ResumeDownloads)

	const jobID = "retry-reset-00000001"

	// Seed the queue with a job carrying the stats a real post-processed
	// job would have recorded — a non-zero DownloadStarted, a ServerStats
	// entry, one failed article — then move it to history as Failed so its
	// per-file progress is retained, and write the NZB backup a retry
	// rebuilds it from.
	ctx := t.Context()
	writeGzNZB(t, h.adminDir, "retry-reset.nzb.gz", retryNZB(2))

	started := time.Now().Add(-10 * time.Minute)
	finished := started.Add(5 * time.Minute)

	seeded, err := queue.NewJob(
		mustParseNZB(t, retryNZB(2)),
		queue.AddOptions{Filename: "retry-reset.nzb"},
		fsutil.SanitizeOptions{},
	)
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	seeded.ID = jobID
	seeded.Name = "retry-reset"
	seeded.NZBBackup = "retry-reset.nzb.gz"

	q := h.app.Queue()
	if err := q.Add(seeded); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.PromoteNext(ctx)
	if err := q.MarkJobStarted(jobID, started); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	if err := q.RecordDownload(jobID, "mock", 123456); err != nil {
		t.Fatalf("RecordDownload: %v", err)
	}
	// One article succeeds and one fails. The successful one is what makes
	// the assertions below non-vacuous: it must survive the retry, which
	// only a loaded overlay can achieve.
	if err := q.MarkArticlesDone(jobID, []string{"a1@t"}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if _, err := q.MarkArticlesFailed(jobID, []string{"a2@t"}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	seeded.MarkDownloadFinished(finished)
	if err := q.Save(h.adminDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	store := q.Store()
	if store == nil {
		t.Fatal("scenario harness queue has no store; retained progress cannot be exercised")
	}
	if err := store.MoveToHistory(ctx, seeded, history.Entry{
		NzoID:     jobID,
		Name:      "retry-reset",
		NzbName:   "retry-reset.nzb",
		NZBBackup: "retry-reset.nzb.gz",
		Status:    string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}
	if err := q.Remove(jobID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	h.app.PauseDownloads()
	h.app.Queue().PauseAll()

	if err := h.app.RetryHistoryJob(ctx, jobID); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	snap := h.app.Queue().SnapshotJob(jobID)
	if snap == nil {
		t.Fatalf("job %s not in queue after retry", jobID)
	}
	if !snap.Progress().DownloadStarted().IsZero() {
		t.Errorf("DownloadStarted = %v, want zero", snap.Progress().DownloadStarted())
	}
	if !snap.Progress().DownloadFinished().IsZero() {
		t.Errorf("DownloadFinished = %v, want zero", snap.Progress().DownloadFinished())
	}
	if stats := snap.Progress().ServerStats(); len(stats) != 0 {
		t.Errorf("ServerStats = %v, want empty", stats)
	}
	if snap.Status != constants.StatusQueued {
		t.Errorf("Status = %q, want Queued", snap.Status)
	}

	// Without these the assertions above are vacuous. A retry rebuilds the
	// job by re-parsing its NZB, so a job whose retained progress failed to
	// load also has zero stats — for the wrong reason.
	//
	// Article 0 succeeded and must survive: that can only happen if the
	// overlay loaded. Article 1 failed and must come back clear: that can
	// only happen if ResetForRetry ran over a loaded overlay. Together they
	// separate "zeroed by reset" from "zeroed by loading nothing".
	if !snap.Progress().ArticleDone(0) {
		t.Error("article 0 succeeded before the failure but is not done after retry; " +
			"the retained overlay never loaded, so the zeroed stats above prove nothing")
	}
	if snap.Progress().ArticleFailed(1) {
		t.Error("article 1 is still marked failed; ResetForRetry did not clear it")
	}
	if snap.Progress().ArticleDone(1) {
		t.Error("article 1 is still marked done; the retry will never refetch it")
	}
}
