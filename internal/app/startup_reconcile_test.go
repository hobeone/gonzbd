package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestStart_RetentionDoesNotDefeatCrashReconciliation pins that enabling
// history retention leaves the startup duplicate-removal working.
//
// Start reconciles a crash between the history commit and Queue.Remove: a
// completed job still sitting in the queue is looked up in history, and if
// the entry is there the stale queue entry is dropped. That lookup is the
// only evidence the job already finished. A retention sweep that runs first
// can delete the entry out from under it, at which point Get returns
// ErrNotFound and the job falls through to maybeFinalize — post-processed and
// re-filed a second time.
//
// The trigger is ordinary: the crash left an entry behind, the daemon stayed
// down past the retention threshold, and the operator restarted it.
func TestStart_RetentionDoesNotDefeatCrashReconciliation(t *testing.T) {
	adminDir := t.TempDir()
	cfg := testConfigInternal(t, adminDir)
	// Short enough that the 30-day-old entry below is expired.
	cfg.General.HistoryRetentionDays = 1

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	application, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A completed job still in the queue — the state a crash between the
	// history commit and Queue.Remove leaves behind.
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "file.bin",
		Bytes:    1024,
		Articles: []nzb.Article{{ID: "rec1@t", Bytes: 1024, Number: 1}},
	}}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "reconcile.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	job.ID = "reconcilestale01"
	job.Status = constants.StatusDownloading
	if err := application.Queue().Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	ackDone(t, application.Queue(), job.ID, "rec1@t")
	// IsComplete keys on the per-file Complete flag, which the assembler
	// sets, not on article state.
	if err := application.Queue().MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	if snap := application.Queue().SnapshotJob(job.ID); snap == nil || !snap.IsComplete() {
		t.Fatal("setup did not produce a completed job in the queue")
	}

	// Its history entry, old enough that retention wants it gone.
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     job.ID,
		Name:      "reconcile",
		Status:    string(constants.StatusCompleted),
		Completed: time.Now().AddDate(0, 0, -30),
	}); err != nil {
		t.Fatalf("seed history entry: %v", err)
	}

	application.PauseDownloads()
	application.Queue().PauseAll()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })

	if application.Queue().SnapshotJob(job.ID) != nil {
		t.Error("the completed job is still queued: retention deleted its history entry " +
			"before reconciliation could look it up, so it will be post-processed again")
	}
}
