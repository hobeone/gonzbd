package app_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
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

	// Seed a history entry and the matching on-disk job state. The
	// persisted job carries the stats a real post-processed job would
	// have recorded: a non-zero DownloadStarted and at least one
	// ServerStats entry.
	ctx := t.Context()
	if err := h.repo.Add(ctx, history.Entry{
		NzoID:  jobID,
		Name:   "retry-reset",
		Status: "Failed",
	}); err != nil {
		t.Fatalf("history.Add: %v", err)
	}

	started := time.Now().Add(-10 * time.Minute)
	finished := started.Add(5 * time.Minute)
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "file.bin", Bytes: 1024, Articles: []nzb.Article{{ID: "a@t", Bytes: 1024, Number: 1}}},
	}}
	persisted, err := queue.NewJob(parsed, queue.AddOptions{Filename: "retry-reset.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	persisted.ID = jobID
	persisted.Name = "retry-reset"
	persisted.Status = constants.StatusFailed
	seedQ := queue.New()
	if err := seedQ.Add(persisted); err != nil {
		t.Fatalf("seedQ.Add: %v", err)
	}
	if err := seedQ.MarkJobStarted(jobID, started); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	if err := seedQ.RecordDownload(jobID, "mock", 123456); err != nil {
		t.Fatalf("RecordDownload: %v", err)
	}
	if _, err := seedQ.MarkArticlesFailed(jobID, []string{"a@t"}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	persisted.MarkDownloadFinished(finished)

	jobPath := filepath.Join(h.adminDir, "history", "jobs", jobID+".json.gz")
	if err := queue.SaveJob(jobPath, persisted); err != nil {
		t.Fatalf("queue.SaveJob: %v", err)
	}
	h.app.PauseDownloads()
	h.app.Queue().PauseAll()

	if err := h.app.RetryHistoryJob(ctx, jobID); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	snap, err := h.app.Queue().SnapshotJob(jobID)
	if err != nil {
		t.Fatalf("job %s not in queue after retry: %v", jobID, err)
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
}
