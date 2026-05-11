package rss

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSave_AtomicWithFsync verifies that Save produces a correct, durable
// file and leaves no temporary files behind.  This is the regression test
// for M12: the fsync before rename ensures that the data is on stable
// storage before the file becomes visible under its final name.
func TestSave_AtomicWithFsync(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	// Record several items so the file has meaningful content.
	for _, id := range []string{"id-1", "id-2", "id-3"} {
		s.Record(id)
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify no temp files remain in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}

	// Reload and verify all IDs survived.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore reload: %v", err)
	}
	for _, id := range []string{"id-1", "id-2", "id-3"} {
		if !s2.Seen(id) {
			t.Errorf("id %q missing after reload", id)
		}
	}
}

// TestSave_NotDirtyNoWrite verifies Save is a no-op when nothing changed.
func TestSave_NotDirtyNoWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	// Save without recording anything — no file should be created.
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("file should not be created when store is not dirty")
	}
}

// TestSave_OverwriteExisting verifies that a second Save replaces the file
// atomically with updated content.
func TestSave_OverwriteExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	s.Record("first")
	if err := s.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	s.Record("second")
	if err := s.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if string(original) == string(updated) {
		t.Error("second Save should produce different content")
	}
	if !strings.Contains(string(updated), "second") {
		t.Error("updated file should contain 'second'")
	}
}
