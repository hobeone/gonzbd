package app_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
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
	h.app.Dispatcher().Pause()
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

	parsed := mustParseNZB(t, retryNZB(2))
	seeded, hdr := buildTestJob(t, h.cfg, parsed, types.FetchOptions{
		JobID:   jobID,
		NzbName: "retry-reset.nzb",
	})
	hdr.Name = "retry-reset"
	hdr.NZBBackup = "retry-reset.nzb.gz"

	if err := h.app.Dispatcher().Add(seeded, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_ = seeded.BeginAttempt(started)
	_ = seeded.RecordDownload("mock", 123456)
	_ = seeded.MarkArticleDone(0, 100, "mock")
	if _, err := durability.NewSQLiteRunStore(h.repo.DB()).Commit(ctx, jobID,
		[]durability.DurableArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
		}); err != nil {
		t.Fatalf("record the durable run: %v", err)
	}
	_ = seeded.MarkArticleFailed(1)
	_, _ = seeded.Finish(job.OutcomeFailed, finished)

	entry := history.Entry{
		NzoID:     jobID,
		Name:      "retry-reset",
		NzbName:   "retry-reset.nzb",
		NZBBackup: "retry-reset.nzb.gz",
		Status:    string(constants.StatusFailed),
	}
	if err := h.app.TriggerPersistAndCommit(slog.Default(), entry, &postproc.Job{Job: seeded}); err != nil {
		t.Fatalf("TriggerPersistAndCommit: %v", err)
	}

	h.app.PauseDownloads()
	h.app.Dispatcher().Pause()

	if err := h.app.RetryHistoryJob(ctx, jobID); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	snap, ok := h.app.Dispatcher().Job(jobID)
	if !ok {
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
	row, ok := h.app.Dispatcher().Row(jobID)
	if !ok {
		t.Fatalf("job %s not in dispatcher", jobID)
	}
	status := job.ToSABnzbd(row.View)
	if status != constants.StatusPaused && status != constants.StatusQueued {
		t.Errorf("Status = %q, want Paused or Queued", status)
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
