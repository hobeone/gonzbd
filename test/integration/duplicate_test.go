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
	a := NewTestApp(t, srv.Addr())

	payload := []byte("dummy content")
	files := []TestFile{{Name: "duplicate.bin", Payload: payload}}
	rawNZB := BuildNZB(files)

	// 1. Add first NZB
	addNZBJob(t, a, rawNZB, "duplicate")
	
	// 2. Add same NZB again (same filename trigger)
	job2 := addNZBJob(t, a, rawNZB, "duplicate")

	// 3. Verify it is paused and has duplicate warning
	if job2.Status != constants.StatusPaused {
		t.Errorf("duplicate job status = %q, want Paused", job2.Status)
	}
	if job2.Warning != "Duplicate NZB" {
		t.Errorf("duplicate job warning = %q, want 'Duplicate NZB'", job2.Warning)
	}

	// 4. Add same NZB again with DIFFERENT filename (MD5 trigger)
	job3 := addNZBJob(t, a, rawNZB, "duplicate-renamed")
	if job3.Status != constants.StatusPaused {
		t.Errorf("MD5 duplicate job status = %q, want Paused", job3.Status)
	}
	if job3.Warning != "Duplicate NZB" {
		t.Errorf("MD5 duplicate job warning = %q, want 'Duplicate NZB'", job3.Warning)
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
