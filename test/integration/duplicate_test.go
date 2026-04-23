//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/constants"
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
	backupPath := filepath.Join(nzbBackupDir, "duplicate.nzb")
	if _, err := os.Stat(backupPath); err != nil {
		t.Errorf("NZB backup missing: %v", err)
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

	// Verify NO new copy was created for the duplicate (job2.Name would be duplicate.1)
	secondBackupPath := filepath.Join(nzbBackupDir, job2.Name+".nzb")
	if _, err := os.Stat(secondBackupPath); err == nil {
		t.Errorf("unexpected backup created for duplicate: %s", secondBackupPath)
	}

	// 4. Add same NZB again with DIFFERENT filename (MD5 trigger)
	// It should still be detected as a duplicate and NOT saved.
	job3 := addNZBJob(t, a, rawNZB, "duplicate-renamed")
	if job3.Status != constants.StatusPaused {
		t.Errorf("MD5 duplicate job status = %q, want Paused", job3.Status)
	}
	
	thirdBackupPath := filepath.Join(nzbBackupDir, "duplicate-renamed.nzb")
	if _, err := os.Stat(thirdBackupPath); err == nil {
		t.Errorf("unexpected backup created for MD5 duplicate: %s", thirdBackupPath)
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
