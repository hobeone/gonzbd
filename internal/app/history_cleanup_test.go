package app_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/history"
)

// newCleanupTestApp builds an Application with a real history repo and
// returns it alongside the repo and admin dir.
func newCleanupTestApp(t *testing.T) (*app.Application, *history.Repository, string) {
	t.Helper()
	adminDir := t.TempDir()
	cfg := testConfig(t.TempDir(), t.TempDir(), adminDir, config.ServerConfig{
		Name: "mock", Host: "127.0.0.1", Port: 1119, Enable: false,
	})
	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	application, err := app.New(cfg, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	return application, repo, adminDir
}

// writeBackupFor creates a history entry with an NZB backup file on disk.
func writeBackupFor(t *testing.T, repo *history.Repository, adminDir, nzoID, backup string) string {
	t.Helper()
	nzbDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbDir, 0o750); err != nil {
		t.Fatalf("mkdir nzb: %v", err)
	}
	path := filepath.Join(nzbDir, backup)
	if err := os.WriteFile(path, []byte("gz placeholder"), 0o600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     nzoID,
		Name:      "cleanup-job",
		Status:    "Failed",
		NzbName:   "cleanup-job.nzb",
		NZBBackup: backup,
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	return path
}

// TestRemoveHistoryJob_DeletesNZBBackup pins that deleting a history entry
// takes its NZB backup with it. The backup is retained solely so that entry
// can be retried, so an entry that outlives nothing must not leave a file
// behind — that accumulation is the whole defect this replaces.
func TestRemoveHistoryJob_DeletesNZBBackup(t *testing.T) {
	application, repo, adminDir := newCleanupTestApp(t)
	path := writeBackupFor(t, repo, adminDir, "cleanupjob000001", "cleanup-job.nzb.gz")

	if err := application.RemoveHistoryJob(t.Context(), "cleanupjob000001", false); err != nil {
		t.Fatalf("RemoveHistoryJob: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("NZB backup survived history deletion, stat err=%v", err)
	}
}

// TestRemoveHistoryJob_ToleratesMissingBackup pins that an entry with no
// backup — written before the name was recorded, or whose file was removed
// by hand — still deletes cleanly. The backup was never required for the
// download, so its absence must not block removing the entry.
func TestRemoveHistoryJob_ToleratesMissingBackup(t *testing.T) {
	application, repo, adminDir := newCleanupTestApp(t)
	path := writeBackupFor(t, repo, adminDir, "cleanupjob000002", "gone.nzb.gz")
	if err := os.Remove(path); err != nil {
		t.Fatalf("pre-remove backup: %v", err)
	}

	if err := application.RemoveHistoryJob(t.Context(), "cleanupjob000002", false); err != nil {
		t.Fatalf("RemoveHistoryJob with missing backup: %v", err)
	}
	if _, err := repo.Get(t.Context(), "cleanupjob000002"); err == nil {
		t.Error("history entry survived deletion")
	}
}
