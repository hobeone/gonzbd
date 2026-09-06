package app_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
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
func addBackupTestJob(t *testing.T, cfg *config.Config, filename, articleID string) (*job.Job, dispatch.Header, []byte) {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "file.bin",
		Bytes:    1024,
		Articles: []nzb.Article{{ID: articleID, Bytes: 1024, Number: 1}},
	}}}
	j, hdr, err := app.BuildIngestJob(cfg, parsed, filename, types.FetchOptions{NzbName: filename}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	return j, hdr, []byte("<nzb>" + articleID + "</nzb>")
}

// TestAddJob_RecordsNZBBackupName pins that AddJob records the basename of
// the backup it actually wrote. Retry resolves admin/nzb/<basename>, so an
// unrecorded name makes the job unretryable even though the file is present.
func TestAddJob_RecordsNZBBackupName(t *testing.T) {
	t.Parallel()
	application, adminDir := newBackupTestApp(t)

	j, hdr, raw := addBackupTestJob(t, application.GetConfig(), "Show.S01E01.nzb", "a@t")
	if err := application.AddJob(t.Context(), j, hdr, raw, false); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	row, ok := application.Dispatcher().Row(j.ID())
	if !ok {
		t.Fatalf("job not found in dispatcher")
	}
	if row.Header.NZBBackup != "Show.S01E01.nzb.gz" {
		t.Errorf("NZBBackup = %q, want %q", row.Header.NZBBackup, "Show.S01E01.nzb.gz")
	}
	if _, err := os.Stat(filepath.Join(adminDir, "nzb", row.Header.NZBBackup)); err != nil {
		t.Errorf("backup not written at recorded name: %v", err)
	}
}

// TestAddJob_ForcedDuplicateKeepsBothBackups pins that a forced duplicate add
// gets a backup of its own rather than none.
func TestAddJob_ForcedDuplicateKeepsBothBackups(t *testing.T) {
	t.Parallel()
	application, adminDir := newBackupTestApp(t)

	first, firstHdr, firstRaw := addBackupTestJob(t, application.GetConfig(), "Show.S01E01.nzb", "a@t")
	if err := application.AddJob(t.Context(), first, firstHdr, firstRaw, false); err != nil {
		t.Fatalf("AddJob first: %v", err)
	}

	second, secondHdr, secondRaw := addBackupTestJob(t, application.GetConfig(), "Show.S01E01.nzb", "b@t")
	if err := application.AddJob(t.Context(), second, secondHdr, secondRaw, true); err != nil {
		t.Fatalf("AddJob second (forced): %v", err)
	}

	firstRow, ok := application.Dispatcher().Row(first.ID())
	if !ok {
		t.Fatal("first job not found in dispatcher")
	}
	secondRow, ok := application.Dispatcher().Row(second.ID())
	if !ok {
		t.Fatal("second job not found in dispatcher")
	}

	if secondRow.Header.NZBBackup == "" {
		t.Fatal("forced duplicate recorded no NZB backup, so it cannot be retried")
	}
	if secondRow.Header.NZBBackup == firstRow.Header.NZBBackup {
		t.Fatalf("both jobs recorded backup %q; the second overwrote the first", firstRow.Header.NZBBackup)
	}

	for _, name := range []string{firstRow.Header.NZBBackup, secondRow.Header.NZBBackup} {
		if _, err := os.Stat(filepath.Join(adminDir, "nzb", name)); err != nil {
			t.Errorf("backup %q missing: %v", name, err)
		}
	}
}
