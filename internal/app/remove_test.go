package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// newRemoveJobTestApp builds an Application wired for RemoveJob tests:
// a fresh download/admin dir pair, an open history repo, and default config.
func newRemoveJobTestApp(t *testing.T) *Application {
	t.Helper()

	dir := t.TempDir()
	downloadDir := filepath.Join(dir, "download")
	adminDir := filepath.Join(dir, "admin")
	if err := os.MkdirAll(downloadDir, 0o750); err != nil {
		t.Fatalf("create download directory: %v", err)
	}
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		t.Fatalf("create admin directory: %v", err)
	}

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("open history database: %v", err)
	}
	repo := history.NewRepository(db)
	cfg := testConfig(
		downloadDir,
		filepath.Join(dir, "complete"),
		adminDir,
		config.ServerConfig{Name: "test"},
	)
	a, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New application failed: %v", err)
	}
	return a
}

func TestRemoveJob(t *testing.T) {
	a := newRemoveJobTestApp(t)
	downloadDir := a.config.GetGeneral().DownloadDir

	parsed := &nzb.NZB{}
	job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "to-delete"}, fsutil.SanitizeOptions{})
	_ = a.queue.Add(job)

	// Create a dummy download directory
	jobDir := filepath.Join(downloadDir, "to-delete")
	_ = os.MkdirAll(jobDir, 0o750)
	dummyFile := filepath.Join(jobDir, "data.bin")
	_ = os.WriteFile(dummyFile, []byte("data"), 0o600)

	// 1. Remove with deleteFiles=true
	err := a.RemoveJob(t.Context(), job.ID, true)
	if err != nil {
		t.Fatalf("RemoveJob failed: %v", err)
	}
	if a.queue.Len() != 0 {
		t.Errorf("queue len = %d, want 0", a.queue.Len())
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Errorf("job directory was NOT deleted but should have been")
	}

	// 2. Add again and remove
	job2, _ := queue.NewJob(parsed, queue.AddOptions{Name: "to-delete-files"}, fsutil.SanitizeOptions{})
	_ = a.queue.Add(job2)
	jobDir2 := filepath.Join(downloadDir, "to-delete-files")
	_ = os.MkdirAll(jobDir2, 0o750)

	err = a.RemoveJob(t.Context(), job2.ID, false)
	if err != nil {
		t.Fatalf("RemoveJob failed: %v", err)
	}
	if _, err := os.Stat(jobDir2); os.IsNotExist(err) {
		t.Errorf("job directory was deleted but should have been kept (deleteFiles=false)")
	}
}

// TestRemoveJob_NilDownloader pins the nil-guard on RemoveJob's final
// DisconnectAll call. New() always wires a real downloader, so app.downloader
// is nilled out directly (white-box, same package) after construction to
// exercise the branch — the same technique TestBuildDownloaderOptions_Defaults
// uses elsewhere in this package. Before the #98 fix this call was
// unsynchronized and unconditional (app.downloader.DisconnectAll()), which
// would have panicked here.
func TestRemoveJob_NilDownloader(t *testing.T) {
	a := newRemoveJobTestApp(t)
	a.downloader = nil

	parsed := &nzb.NZB{}
	job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "to-delete"}, fsutil.SanitizeOptions{})
	_ = a.queue.Add(job)

	if err := a.RemoveJob(t.Context(), job.ID, false); err != nil {
		t.Fatalf("RemoveJob with nil downloader: %v", err)
	}
}
