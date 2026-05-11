package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSave_DoesNotHoldLockDuringIO verifies that Save releases the RLock
// before writing to disk. Under the old defer-RUnlock pattern, a concurrent
// With (write-lock) call would deadlock. This test detects that regression
// by running Save and With concurrently with a timeout.
func TestSave_DoesNotHoldLockDuringIO(t *testing.T) {
	t.Parallel()
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gonzbd.yaml")

	// Run Save and With concurrently. Under the old code (RLock held during
	// the full I/O sequence), With would block until Save's I/O completed.
	// With the fix (snapshot under RLock then release), both can proceed
	// without contention on the write lock.
	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 10 {
			if err := cfg.Save(path); err != nil {
				t.Errorf("Save: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 10 {
			cfg.With(func(c *Config) {
				c.General.Port = 9999
			})
		}
	}()

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Good: no deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock detected: Save likely holds RLock during disk I/O")
	}
}

// TestSave_RoundTripWithSnapshotPattern verifies that the refactored Save
// (snapshot to buffer, release lock, write) still produces valid YAML that
// round-trips correctly.
func TestSave_RoundTripWithSnapshotPattern(t *testing.T) {
	t.Parallel()
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.General.Port = 7777

	dir := t.TempDir()
	path := filepath.Join(dir, "gonzbd.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.General.Port != 7777 {
		t.Errorf("Port = %d, want 7777", loaded.General.Port)
	}

	// Full YAML round-trip comparison.
	want := marshalForCompare(t, cfg)
	got := marshalForCompare(t, loaded)
	if !bytes.Equal(want, got) {
		t.Fatalf("round-trip diverged:\n--- original ---\n%s\n--- loaded ---\n%s", want, got)
	}
}

// TestSave_NoTempFileLeftBehind verifies atomic cleanup on success.
func TestSave_NoTempFileLeftBehind(t *testing.T) {
	t.Parallel()
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gonzbd.yaml")

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}
