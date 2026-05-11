package dirscanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSave_AtomicWithFsync verifies that Save produces a correct, durable
// file and leaves no temporary files behind.  This is the regression test
// for M24: the fsync before rename ensures durability.
func TestSave_AtomicWithFsync(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	for i := range 5 {
		s.Set(filepath.Join("/downloads", "file"+string(rune('A'+i))+".nzb"),
			FileState{Size: int64(i * 1000), MTime: now})
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify no temp files remain.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}

	// Reload and verify all entries survived.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore reload: %v", err)
	}
	for i := range 5 {
		key := filepath.Join("/downloads", "file"+string(rune('A'+i))+".nzb")
		state, ok := s2.Get(key)
		if !ok {
			t.Errorf("key %q missing after reload", key)
			continue
		}
		if state.Size != int64(i*1000) {
			t.Errorf("key %q: size = %d, want %d", key, state.Size, i*1000)
		}
	}
}

// TestSave_OverwriteExisting verifies that a second Save atomically
// replaces the file with updated content.
func TestSave_OverwriteExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	s.Set("file1.nzb", FileState{Size: 100, MTime: time.Now()})
	if err := s.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	s.Set("file2.nzb", FileState{Size: 200, MTime: time.Now()})
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
	if !strings.Contains(string(updated), "file2.nzb") {
		t.Error("updated file should contain file2.nzb")
	}
}
