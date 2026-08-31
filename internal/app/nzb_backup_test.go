package app_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// newBackupTestApp builds an Application over temp dirs with a real history
// repo, which AddJob needs for its MD5 duplicate probe.
func newBackupTestApp(t *testing.T) (*app.Application, string) {
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
	application, err := app.New(cfg, history.NewRepository(db))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	return application, adminDir
}

// addBackupTestJob builds a job from a one-article NZB with the given
// filename and article ID. A distinct article ID gives the job a distinct
// MD5, so the MD5 duplicate probe does not fire and the filename path is the
// one under test.
func addBackupTestJob(t *testing.T, filename, articleID string) (*queue.Job, []byte) {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "file.bin",
		Bytes:    1024,
		Articles: []nzb.Article{{ID: articleID, Bytes: 1024, Number: 1}},
	}}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: filename}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	return job, []byte("<nzb>" + articleID + "</nzb>")
}

// TestAddJob_RecordsNZBBackupName pins that AddJob records the basename of
// the backup it actually wrote. Retry resolves admin/nzb/<basename>, so an
// unrecorded name makes the job unretryable even though the file is present.
func TestAddJob_RecordsNZBBackupName(t *testing.T) {
	t.Parallel()
	application, adminDir := newBackupTestApp(t)

	job, raw := addBackupTestJob(t, "Show.S01E01.nzb", "a@t")
	if err := application.AddJob(t.Context(), job, raw, false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	if job.NZBBackup != "Show.S01E01.nzb.gz" {
		t.Errorf("NZBBackup = %q, want %q", job.NZBBackup, "Show.S01E01.nzb.gz")
	}
	if _, err := os.Stat(filepath.Join(adminDir, "nzb", job.NZBBackup)); err != nil {
		t.Errorf("backup not written at recorded name: %v", err)
	}
}

// TestAddJob_ForcedDuplicateKeepsBothBackups pins that a forced duplicate add
// gets a backup of its own rather than none.
//
// Before this change AddJob wrote the backup only when the job was not a
// duplicate, so a forced re-add downloaded normally and finalized with no NZB
// on disk at all — unretryable, and silently so. The suffix keeps the
// original file intact, since admin/nzb/ is browsable by the name the job was
// submitted under and overwriting would lose the first NZB.
func TestAddJob_ForcedDuplicateKeepsBothBackups(t *testing.T) {
	t.Parallel()
	application, adminDir := newBackupTestApp(t)

	first, firstRaw := addBackupTestJob(t, "Show.S01E01.nzb", "a@t")
	if err := application.AddJob(t.Context(), first, firstRaw, false); err != nil {
		t.Fatalf("AddJob first: %v", err)
	}

	second, secondRaw := addBackupTestJob(t, "Show.S01E01.nzb", "b@t")
	if err := application.AddJob(t.Context(), second, secondRaw, true); err != nil {
		t.Fatalf("AddJob second (forced): %v", err)
	}

	if second.NZBBackup == "" {
		t.Fatal("forced duplicate recorded no NZB backup, so it cannot be retried")
	}
	if second.NZBBackup == first.NZBBackup {
		t.Fatalf("both jobs recorded backup %q; the second overwrote the first", first.NZBBackup)
	}

	for _, name := range []string{first.NZBBackup, second.NZBBackup} {
		if _, err := os.Stat(filepath.Join(adminDir, "nzb", name)); err != nil {
			t.Errorf("backup %q missing: %v", name, err)
		}
	}
}
