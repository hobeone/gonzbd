package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

func TestRemoveJob(t *testing.T) {
	dir := t.TempDir()
	downloadDir := filepath.Join(dir, "download")
	adminDir := filepath.Join(dir, "admin")
	_ = os.MkdirAll(downloadDir, 0o750)
	_ = os.MkdirAll(adminDir, 0o750)

	db, _ := history.Open(filepath.Join(adminDir, "history.db"))
	repo := history.NewRepository(db)
	a, err := New(Config{
		DownloadDir: downloadDir,
		CompleteDir: filepath.Join(dir, "complete"),
		AdminDir:    adminDir,
		Servers:     []config.ServerConfig{{Name: "test"}},
	}, repo)
	if err != nil {
		t.Fatalf("New application failed: %v", err)
	}

	parsed := &nzb.NZB{}
	job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "to-delete"})
	_ = a.queue.Add(job)

	// Create a dummy download directory
	jobDir := filepath.Join(downloadDir, "to-delete")
	_ = os.MkdirAll(jobDir, 0o750)
	dummyFile := filepath.Join(jobDir, "data.bin")
	_ = os.WriteFile(dummyFile, []byte("data"), 0o600)

	// 1. Remove (always deletes files in our current implementation)
	err = a.RemoveJob(job.ID)
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
	job2, _ := queue.NewJob(parsed, queue.AddOptions{Name: "to-delete-files"})
	_ = a.queue.Add(job2)
	jobDir2 := filepath.Join(downloadDir, "to-delete-files")
	_ = os.MkdirAll(jobDir2, 0o750)

	err = a.RemoveJob(job2.ID)
	if err != nil {
		t.Fatalf("RemoveJob failed: %v", err)
	}
	if _, err := os.Stat(jobDir2); !os.IsNotExist(err) {
		t.Errorf("job directory still exists but should have been deleted")
	}
}
