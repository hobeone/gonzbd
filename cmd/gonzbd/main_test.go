package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestWaitForCompletion_DoesNotReturnEarlyWhileJobInQueue verifies that
// waitForCompletion does not return nil prematurely on tick.C when a history
// entry exists but the job is still registered in the queue (e.g. while an
// automatic held-volume retry is active).
func TestWaitForCompletion_DoesNotReturnEarlyWhileJobInQueue(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	adminDir := filepath.Join(tmpDir, "admin")
	if err := os.MkdirAll(adminDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(adminDir, "history.db")
	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	repo := history.NewRepository(db)
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.General.AdminDir = adminDir
	cfg.General.DownloadDir = filepath.Join(tmpDir, "download")
	cfg.General.CompleteDir = filepath.Join(tmpDir, "complete")

	application, err := app.New(cfg, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	if err := application.Dispatcher().Restore(t.Context()); err != nil {
		t.Fatalf("Dispatcher().Restore: %v", err)
	}

	const jobID = "test-job-001"
	j := job.New(jobID, "test.nzb", job.Policy{})
	hdr := dispatch.Header{
		Name:     "test",
		Filename: "test.nzb",
	}
	if err := application.Dispatcher().Add(t.Context(), j, hdr); err != nil {
		t.Fatalf("Dispatcher().Add: %v", err)
	}

	// Insert a history entry for the job under the same ID (simulating Attempt 1's
	// Failed row before retry cleanup, or a stale history entry).
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:  jobID,
		Name:   "test",
		Status: "Failed",
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	// Ensure the fixture conditions hold:
	if application.GetJob(j.ID()) == nil {
		t.Fatal("fixture guard: job must be registered in the queue")
	}
	if _, err := application.GetHistory(t.Context(), j.ID()); err != nil {
		t.Fatalf("fixture guard: history entry must exist: %v", err)
	}

	// Run waitForCompletion with a timeout longer than the 2s ticker (2.5s).
	// Because the job is still registered in the queue, waitForCompletion
	// must NOT exit with nil on the 2s ticker tick.
	ctx, cancel := context.WithTimeout(t.Context(), 2500*time.Millisecond)
	defer cancel()

	err = waitForCompletion(ctx, application, j, slog.Default())
	if err == nil {
		t.Fatal("waitForCompletion returned nil while job was still registered in queue")
	}
}
