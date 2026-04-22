package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/queue"
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

	// Pause post-proc so the requeued job sits in the queue long enough
	// to be inspected without racing the finaliser.
	h.app.PausePostProcessor()
	t.Cleanup(h.app.ResumePostProcessor)

	const jobID = "retry-reset-00000001"

	// Seed a history entry and the matching on-disk job state. The
	// persisted job carries the stats a real post-processed job would
	// have recorded: a non-zero DownloadStarted and at least one
	// ServerStats entry.
	ctx := context.Background()
	if err := h.repo.Add(ctx, history.Entry{
		NzoID:  jobID,
		Name:   "retry-reset",
		Status: "Failed",
	}); err != nil {
		t.Fatalf("history.Add: %v", err)
	}

	started := time.Now().Add(-10 * time.Minute)
	persisted := &queue.Job{
		ID:              jobID,
		Name:            "retry-reset",
		Status:          constants.StatusFailed,
		DownloadStarted: started,
		ServerStats:     map[string]int64{"mock": 123456},
		Files: []queue.JobFile{{
			Subject:  "file.bin",
			Complete: false,
			Articles: []queue.JobArticle{{ID: "a@t", Done: false, Failed: true, Bytes: 1024}},
		}},
		FailedBytes: 1024,
	}
	jobPath := filepath.Join(h.adminDir, "queue", "jobs", jobID+".json.gz")
	if err := queue.SaveJob(jobPath, persisted); err != nil {
		t.Fatalf("queue.SaveJob: %v", err)
	}

	if err := h.app.RetryHistoryJob(ctx, jobID); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	snap := h.app.Queue().SnapshotJob(jobID)
	if snap == nil {
		t.Fatalf("job %s not in queue after retry", jobID)
	}
	if !snap.DownloadStarted.IsZero() {
		t.Errorf("DownloadStarted = %v, want zero", snap.DownloadStarted)
	}
	if len(snap.ServerStats) != 0 {
		t.Errorf("ServerStats = %v, want empty", snap.ServerStats)
	}
	if snap.Status != constants.StatusQueued {
		t.Errorf("Status = %q, want %q", snap.Status, constants.StatusQueued)
	}
}
