package app_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// newRetentionApp builds an Application with the given retention thresholds
// and a real history repo.
func newRetentionApp(t *testing.T, retainDays, retainFailedDays int) (*app.Application, *history.Repository, string) {
	t.Helper()
	adminDir := t.TempDir()
	cfg := testConfig(t.TempDir(), t.TempDir(), adminDir, config.ServerConfig{
		Name: "mock", Host: "127.0.0.1", Port: 1119, Enable: false,
	})
	cfg.General.HistoryRetentionDays = retainDays
	cfg.General.HistoryFailedRetentionDays = retainFailedDays

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

// seedEntry writes a history entry with an NZB backup on disk.
func seedEntry(t *testing.T, repo *history.Repository, adminDir, id, status string, ageDays int) string {
	t.Helper()
	nzbDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbDir, 0o750); err != nil {
		t.Fatalf("mkdir nzb: %v", err)
	}
	backup := id + ".nzb.gz"
	path := filepath.Join(nzbDir, backup)
	if err := os.WriteFile(path, []byte("gz placeholder"), 0o600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      id,
		Status:    status,
		NZBBackup: backup,
		Completed: time.Now().AddDate(0, 0, -ageDays),
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	return path
}

// TestPruneHistory_RemovesExpiredEntriesAndTheirBackups pins that retention
// releases the NZB backup along with the entry.
//
// The backup is a file, so Repository.Delete cannot remove it — only the
// app-level path can. A pruner that swept rows and left the backups would
// grow admin/nzb/ without bound, which is the same leak the payload format
// had before #298.
func TestPruneHistory_RemovesExpiredEntriesAndTheirBackups(t *testing.T) {
	application, repo, adminDir := newRetentionApp(t, 30, 0)

	oldPath := seedEntry(t, repo, adminDir, "pruneold00000001", string(constants.StatusCompleted), 90)
	freshPath := seedEntry(t, repo, adminDir, "prunefresh000001", string(constants.StatusCompleted), 1)

	n, err := application.PruneHistory(t.Context())
	if err != nil {
		t.Fatalf("PruneHistory: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d entries, want 1", n)
	}

	if _, err := repo.Get(t.Context(), "pruneold00000001"); err == nil {
		t.Error("expired entry survived the sweep")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expired entry's NZB backup survived, stat err = %v", err)
	}

	if _, err := repo.Get(t.Context(), "prunefresh000001"); err != nil {
		t.Errorf("entry inside the retention window was swept: %v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("surviving entry's backup was removed: %v", err)
	}
}

// TestPruneHistory_KeepsFailedJobsWhenOnlySuccessesExpire pins the status
// split at the app level. A short window on successes must not sweep away a
// failed job, which is the one an operator keeps in order to retry it — and
// retrying needs the backup this would have deleted.
func TestPruneHistory_KeepsFailedJobsWhenOnlySuccessesExpire(t *testing.T) {
	application, repo, adminDir := newRetentionApp(t, 30, 0)

	failedPath := seedEntry(t, repo, adminDir, "prunefailed00001", string(constants.StatusFailed), 90)
	seedEntry(t, repo, adminDir, "prunedone0000001", string(constants.StatusCompleted), 90)

	if _, err := application.PruneHistory(t.Context()); err != nil {
		t.Fatalf("PruneHistory: %v", err)
	}

	if _, err := repo.Get(t.Context(), "prunefailed00001"); err != nil {
		t.Errorf("failed entry swept although retain_failed_days is 0 (keep forever): %v", err)
	}
	if _, err := os.Stat(failedPath); err != nil {
		t.Errorf("failed entry's backup removed, so it can no longer be retried: %v", err)
	}
	if _, err := repo.Get(t.Context(), "prunedone0000001"); err == nil {
		t.Error("expired completed entry survived")
	}
}

// TestPruneHistory_DefaultKeepsEverything pins that an operator who has not
// configured retention loses nothing. Zero thresholds are the default, so a
// mistake here deletes history on every startup of every unconfigured
// install.
func TestPruneHistory_DefaultKeepsEverything(t *testing.T) {
	application, repo, adminDir := newRetentionApp(t, 0, 0)

	path := seedEntry(t, repo, adminDir, "prunenever000001", string(constants.StatusCompleted), 3650)

	n, err := application.PruneHistory(t.Context())
	if err != nil {
		t.Fatalf("PruneHistory: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned %d entries with retention disabled, want 0", n)
	}
	if _, err := repo.Get(t.Context(), "prunenever000001"); err != nil {
		t.Errorf("a ten-year-old entry was swept with retention disabled: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("backup removed with retention disabled: %v", err)
	}
}
