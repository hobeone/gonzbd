//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

func TestIntegration_DuplicateDetection(t *testing.T) {
	srv := newMockServer(t, nil)
	// We need a stable directory to check the admin/nzb folder
	dir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), dir)

	payload := []byte("dummy content")
	files := []TestFile{{Name: "duplicate.bin", Payload: payload}}
	rawNZB := BuildNZB(files)

	// 1. Add first NZB
	addNZBJob(t, a, rawNZB, "duplicate")

	// Verify NZB backup exists under the original filename
	nzbBackupDir := filepath.Join(dir, "admin", "queue", "nzb")
	// Wait, newTestAppWithDir sets AdminDir = downloadDir (which is 'dir' here)
	// Actually buildAppConfig in testhelpers_test.go:
	// AdminDir: downloadDir
	// And AddJob uses filepath.Join(app.cfg.AdminDir, "nzb")
	nzbBackupDir = filepath.Join(dir, "nzb")
	backupPath := filepath.Join(nzbBackupDir, "duplicate.nzb.gz")
	if _, err := os.Stat(backupPath); err != nil {
		t.Errorf("NZB gzip backup missing: %v", err)
	}

	// 2. Add same NZB again (same filename trigger)
	job2 := addNZBJob(t, a, rawNZB, "duplicate")

	// 3. Verify it is paused and has duplicate warning
	if job2.Status != constants.StatusPaused {
		t.Errorf("duplicate job status = %q, want Paused", job2.Status)
	}
	if job2.Warning != "Duplicate NZB" {
		t.Errorf("duplicate job warning = %q, want 'Duplicate NZB'", job2.Warning)
	}

	// A duplicate now gets a backup of its own, under a suffixed name.
	//
	// This reverses the earlier behaviour of skipping the write. The backup
	// is the only surviving copy of the article message-IDs once a job's
	// manifest is unlinked at finalization, so a job without one cannot be
	// retried — and a non-forced duplicate is enqueued paused, meaning the
	// operator can resume it, download it, and have it fail. The original
	// file is left intact rather than overwritten, because admin/nzb/ is
	// browsed by hand for the name a job was submitted under.
	if job2.NZBBackup == "" {
		t.Error("duplicate job recorded no NZB backup, so it cannot be retried if resumed")
	}
	if job2.NZBBackup == "duplicate.nzb.gz" {
		t.Error("duplicate job reused the first job's backup name; the original would be overwritten")
	}
	if _, err := os.Stat(filepath.Join(nzbBackupDir, job2.NZBBackup)); err != nil {
		t.Errorf("duplicate job's backup %q missing: %v", job2.NZBBackup, err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Errorf("first job's backup was clobbered by the duplicate: %v", err)
	}

	// 4. Add same NZB again with DIFFERENT filename (MD5 trigger).
	// Still detected as a duplicate, still paused.
	job3 := addNZBJob(t, a, rawNZB, "duplicate-renamed")
	if job3.Status != constants.StatusPaused {
		t.Errorf("MD5 duplicate job status = %q, want Paused", job3.Status)
	}

	// Same reasoning as above: paused, but resumable, so it needs a backup.
	// This one takes its own filename with no suffix, since nothing else
	// claims it.
	if job3.NZBBackup != "duplicate-renamed.nzb.gz" {
		t.Errorf("MD5 duplicate NZBBackup = %q, want %q", job3.NZBBackup, "duplicate-renamed.nzb.gz")
	}
	if _, err := os.Stat(filepath.Join(nzbBackupDir, "duplicate-renamed.nzb.gz")); err != nil {
		t.Errorf("MD5 duplicate's backup missing: %v", err)
	}
}

func TestIntegration_DirectoryCollision(t *testing.T) {
	srv := newMockServer(t, nil)
	dir := t.TempDir()

	// Pre-create a directory that would collide
	collidingDir := filepath.Join(dir, "collision")
	if err := os.MkdirAll(collidingDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	a := newTestAppWithDir(t, srv.Addr(), dir)

	payload := []byte("dummy content")
	files := []TestFile{{Name: "data.bin", Payload: payload}}
	rawNZB := BuildNZB(files)

	// Add job with name "collision"
	job := addNZBJob(t, a, rawNZB, "collision")

	// Verify job was renamed to collision.1
	if job.Name != "collision.1" {
		t.Errorf("job name = %q, want collision.1", job.Name)
	}
}
