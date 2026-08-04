package app_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/history"
)

// TestRemoveHistoryJob_NoRepositoryWired pins the guard for an Application
// built without history. Returning nil here would report a deletion that
// never happened.
func TestRemoveHistoryJob_NoRepositoryWired(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir(), config.ServerConfig{
		Name: "mock", Host: "127.0.0.1", Port: 1119, Enable: false,
	})
	application, err := app.New(cfg, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	if err := application.RemoveHistoryJob(t.Context(), "nowhere000000001", false); err == nil {
		t.Fatal("removal succeeded with no history repository wired")
	}
}

// TestRemoveHistoryJob_UnknownEntry pins that a missing entry is an error
// rather than a silent no-op, so a client deleting a stale id learns of it.
func TestRemoveHistoryJob_UnknownEntry(t *testing.T) {
	application, _, _ := newCleanupTestApp(t)
	if err := application.RemoveHistoryJob(t.Context(), "missing000000001", false); err == nil {
		t.Fatal("removal of an unknown entry reported success")
	}
}

// TestRemoveHistoryJob_DeletesFilesWhenAsked pins the deleteFiles branch: the
// job's output directory goes when requested, and survives when not.
func TestRemoveHistoryJob_DeletesFilesWhenAsked(t *testing.T) {
	for _, tt := range []struct {
		name        string
		deleteFiles bool
		wantGone    bool
	}{
		{"delete requested", true, true},
		{"delete not requested", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			adminDir := t.TempDir()
			completeDir := t.TempDir()
			cfg := testConfig(t.TempDir(), completeDir, adminDir, config.ServerConfig{
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

			jobDir := filepath.Join(completeDir, "finished-job")
			if err := os.MkdirAll(jobDir, 0o750); err != nil {
				t.Fatalf("mkdir job dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(jobDir, "movie.mkv"), []byte("data"), 0o600); err != nil {
				t.Fatalf("write payload: %v", err)
			}
			if err := repo.Add(t.Context(), history.Entry{
				NzoID: "withfiles0000001", Name: "finished-job", Status: "Completed", Path: jobDir,
			}); err != nil {
				t.Fatalf("repo.Add: %v", err)
			}

			if err := application.RemoveHistoryJob(t.Context(), "withfiles0000001", tt.deleteFiles); err != nil {
				t.Fatalf("RemoveHistoryJob: %v", err)
			}
			_, statErr := os.Stat(jobDir)
			if tt.wantGone && !os.IsNotExist(statErr) {
				t.Errorf("job directory survived deleteFiles=true, stat err = %v", statErr)
			}
			if !tt.wantGone && statErr != nil {
				t.Errorf("job directory removed despite deleteFiles=false: %v", statErr)
			}
		})
	}
}

// TestRemoveHistoryJob_RefusesPathOutsideManagedDirs pins that a Path
// pointing outside the download and complete directories is refused, and
// that refusing it does not block deleting the entry.
//
// entry.Path is written by us but a database on disk is not a trust boundary
// after the fact, and deleteFiles is reachable from the API.
func TestRemoveHistoryJob_RefusesPathOutsideManagedDirs(t *testing.T) {
	application, repo, adminDir := newCleanupTestApp(t)

	outside := filepath.Join(t.TempDir(), "not-ours")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := repo.Add(t.Context(), history.Entry{
		NzoID: "outsidepath00001", Name: "outside", Status: "Failed", Path: outside,
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	if err := application.RemoveHistoryJob(t.Context(), "outsidepath00001", true); err != nil {
		t.Fatalf("RemoveHistoryJob: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a directory outside the managed dirs was deleted: %v", err)
	}
	if _, err := repo.Get(t.Context(), "outsidepath00001"); err == nil {
		t.Error("entry survived; a refused directory delete must not block it")
	}
	_ = adminDir
}

// TestRemoveHistoryJob_UndeletableBackupStillRemovesEntry pins that a backup
// that cannot be unlinked is logged and stepped over rather than failing the
// deletion. The entry is the thing the operator asked to remove; a stranded
// file must not keep it alive.
func TestRemoveHistoryJob_UndeletableBackupStillRemovesEntry(t *testing.T) {
	application, repo, adminDir := newCleanupTestApp(t)

	// A non-empty directory where the backup file belongs: os.Remove fails
	// with something other than "not exist".
	blocker := filepath.Join(adminDir, "nzb", "blocked.nzb.gz")
	if err := os.MkdirAll(blocker, 0o750); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker child: %v", err)
	}
	if err := repo.Add(t.Context(), history.Entry{
		NzoID: "blockedbackup001", Name: "blocked", Status: "Failed", NZBBackup: "blocked.nzb.gz",
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	if err := application.RemoveHistoryJob(t.Context(), "blockedbackup001", false); err != nil {
		t.Fatalf("RemoveHistoryJob: %v", err)
	}
	if _, err := repo.Get(t.Context(), "blockedbackup001"); err == nil {
		t.Error("entry survived because its backup could not be unlinked")
	}
}
