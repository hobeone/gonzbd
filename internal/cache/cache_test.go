package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// helper builds a Cache with a given memory limit in bytes.
func newCache(t *testing.T, limit int64, onPressure func()) *Cache {
	t.Helper()
	return New(Options{Limit: limit, OnPressure: onPressure})
}

// --------------------------------------------------------------------------
// ReserveSpace / FreeReserved
// --------------------------------------------------------------------------

func TestReserveSpace(t *testing.T) {
	t.Run("reserve within limit succeeds and updates Used", func(t *testing.T) {
		c := newCache(t, 100, nil)
		if !c.ReserveSpace(50) {
			t.Fatal("expected ReserveSpace(50) to succeed with limit 100")
		}
		if got := c.Used(); got != 50 {
			t.Fatalf("Used() = %d; want 50", got)
		}
	})

	t.Run("reserve exceeds limit returns false", func(t *testing.T) {
		c := newCache(t, 100, nil)
		if c.ReserveSpace(101) {
			t.Fatal("expected ReserveSpace(101) to fail with limit 100")
		}
		if got := c.Used(); got != 0 {
			t.Fatalf("Used() = %d after failed reserve; want 0", got)
		}
	})

	t.Run("cumulative reservations exhaust limit", func(t *testing.T) {
		c := newCache(t, 100, nil)
		if !c.ReserveSpace(60) {
			t.Fatal("first reserve failed unexpectedly")
		}
		if c.ReserveSpace(50) {
			t.Fatal("second reserve should have failed (60+50 > 100)")
		}
	})

	t.Run("FreeReserved releases space", func(t *testing.T) {
		c := newCache(t, 100, nil)
		if !c.ReserveSpace(100) {
			t.Fatal("reserve 100 failed unexpectedly")
		}
		c.FreeReserved(100)
		if got := c.Used(); got != 0 {
			t.Fatalf("Used() = %d after FreeReserved; want 0", got)
		}
		// Space should now be available again.
		if !c.ReserveSpace(100) {
			t.Fatal("reserve after free failed unexpectedly")
		}
	})

	t.Run("limit 0 always returns false (no-cache mode)", func(t *testing.T) {
		c := newCache(t, 0, nil)
		if c.ReserveSpace(1) {
			t.Fatal("expected ReserveSpace to return false when limit=0")
		}
	})
}

// --------------------------------------------------------------------------
// Save — memory path
// --------------------------------------------------------------------------

func TestSaveMemory(t *testing.T) {
	t.Run("save within limit stays in memory", func(t *testing.T) {
		c := newCache(t, 200, nil)
		data := []byte("hello article")
		if !c.ReserveSpace(int64(len(data))) {
			t.Fatal("reserve failed")
		}
		if err := c.Save("msg1", t.TempDir(), data); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if got := c.Used(); got != int64(len(data)) {
			t.Fatalf("Used() = %d; want %d", got, len(data))
		}
	})

	t.Run("idempotent save replaces entry and adjusts counter", func(t *testing.T) {
		c := newCache(t, 200, nil)
		dir := t.TempDir()
		data1 := []byte("first version")
		data2 := []byte("second longer version")

		// First save.
		if !c.ReserveSpace(int64(len(data1))) {
			t.Fatal("first reserve failed")
		}
		if err := c.Save("msg-idem", dir, data1); err != nil {
			t.Fatalf("first Save: %v", err)
		}

		// Second save — same key, larger data. No ReserveSpace call; cache must
		// still accept it if budget allows after replacing the old entry.
		if !c.ReserveSpace(int64(len(data2))) {
			t.Fatal("second reserve failed")
		}
		if err := c.Save("msg-idem", dir, data2); err != nil {
			t.Fatalf("second Save: %v", err)
		}
		// Used should reflect data2 only.
		if got := c.Used(); got != int64(len(data2)) {
			t.Fatalf("Used() = %d; want %d", got, len(data2))
		}
	})
}

// --------------------------------------------------------------------------
// Save — disk spill path
// --------------------------------------------------------------------------

func TestSaveDisk(t *testing.T) {
	t.Run("save without prior reserve spills to disk", func(t *testing.T) {
		c := newCache(t, 200, nil)
		dir := t.TempDir()
		data := []byte("spilled article")
		// Do NOT call ReserveSpace first.
		if err := c.Save("spill-key", dir, data); err != nil {
			t.Fatalf("Save (spill): %v", err)
		}
		// Memory should be empty.
		if got := c.Used(); got != 0 {
			t.Fatalf("Used() = %d; want 0 after disk spill", got)
		}
		// File should exist on disk.
		path := diskPath(dir, "spill-key")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected spill file at %s: %v", path, err)
		}
	})

	t.Run("limit 0 always spills to disk", func(t *testing.T) {
		c := newCache(t, 0, nil)
		dir := t.TempDir()
		data := []byte("no-cache article")
		if err := c.Save("nc-key", dir, data); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if got := c.Used(); got != 0 {
			t.Fatalf("Used() = %d; want 0", got)
		}
		path := diskPath(dir, "nc-key")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected disk file: %v", err)
		}
	})
}

// --------------------------------------------------------------------------
// Load
// --------------------------------------------------------------------------

func TestLoad(t *testing.T) {
	t.Run("load from memory consumes entry", func(t *testing.T) {
		c := newCache(t, 200, nil)
		dir := t.TempDir()
		data := []byte("in-memory article")
		if !c.ReserveSpace(int64(len(data))) {
			t.Fatal("reserve failed")
		}
		if err := c.Save("load-mem", dir, data); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := c.Load("load-mem", dir)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if string(got) != string(data) {
			t.Fatalf("Load returned %q; want %q", got, data)
		}
		// Counter must be cleared.
		if u := c.Used(); u != 0 {
			t.Fatalf("Used() = %d after Load; want 0", u)
		}
		// Second load should return ErrNotFound.
		if _, err := c.Load("load-mem", dir); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second Load error = %v; want ErrNotFound", err)
		}
	})

	t.Run("load from disk after spill consumes file", func(t *testing.T) {
		c := newCache(t, 200, nil)
		dir := t.TempDir()
		data := []byte("disk article")
		// Spill to disk directly.
		if err := c.Save("disk-key", dir, data); err != nil {
			t.Fatalf("Save (spill): %v", err)
		}
		got, err := c.Load("disk-key", dir)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if string(got) != string(data) {
			t.Fatalf("Load = %q; want %q", got, data)
		}
		// File must be gone.
		path := diskPath(dir, "disk-key")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected file removed after Load, stat err = %v", err)
		}
		// Second load → ErrNotFound.
		if _, err := c.Load("disk-key", dir); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second Load error = %v; want ErrNotFound", err)
		}
	})

	t.Run("load missing key returns ErrNotFound", func(t *testing.T) {
		c := newCache(t, 200, nil)
		_, err := c.Load("nonexistent", t.TempDir())
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
}

// --------------------------------------------------------------------------
// Flush
// --------------------------------------------------------------------------

func TestFlush(t *testing.T) {
	t.Run("flush writes all memory entries to disk and clears map", func(t *testing.T) {
		c := newCache(t, 1024, nil)
		dir := t.TempDir()
		articles := map[string][]byte{
			"art1": []byte("data one"),
			"art2": []byte("data two"),
			"art3": []byte("data three"),
		}
		for key, data := range articles {
			if !c.ReserveSpace(int64(len(data))) {
				t.Fatalf("reserve failed for %s", key)
			}
			if err := c.Save(key, dir, data); err != nil {
				t.Fatalf("Save %s: %v", key, err)
			}
		}
		if err := c.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if got := c.Used(); got != 0 {
			t.Fatalf("Used() = %d after Flush; want 0", got)
		}
		// All files must be on disk.
		for key, want := range articles {
			path := diskPath(dir, key)
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile %s: %v", path, err)
			}
			if string(got) != string(want) {
				t.Fatalf("flush file for %s = %q; want %q", key, got, want)
			}
		}
		// Memory should be empty — Load must now find things on disk.
		for key, want := range articles {
			got, err := c.Load(key, dir)
			if err != nil {
				t.Fatalf("Load after flush %s: %v", key, err)
			}
			if string(got) != string(want) {
				t.Fatalf("Load after flush %s = %q; want %q", key, got, want)
			}
		}
	})
}

// --------------------------------------------------------------------------
// Purge
// --------------------------------------------------------------------------

func TestPurge(t *testing.T) {
	t.Run("purge removes from memory and disk", func(t *testing.T) {
		c := newCache(t, 1024, nil)
		dir := t.TempDir()

		memData := []byte("in memory")
		diskData := []byte("on disk")

		// memKey lives in memory.
		if !c.ReserveSpace(int64(len(memData))) {
			t.Fatal("reserve failed")
		}
		if err := c.Save("memKey", dir, memData); err != nil {
			t.Fatalf("Save memKey: %v", err)
		}

		// diskKey is spilled (no reserve).
		if err := c.Save("diskKey", dir, diskData); err != nil {
			t.Fatalf("Save diskKey: %v", err)
		}

		if err := c.Purge([]string{"memKey", "diskKey"}, dir); err != nil {
			t.Fatalf("Purge: %v", err)
		}

		if got := c.Used(); got != 0 {
			t.Fatalf("Used() = %d after Purge; want 0", got)
		}

		if _, err := c.Load("memKey", dir); !errors.Is(err, ErrNotFound) {
			t.Fatalf("memKey Load after Purge = %v; want ErrNotFound", err)
		}
		if _, err := c.Load("diskKey", dir); !errors.Is(err, ErrNotFound) {
			t.Fatalf("diskKey Load after Purge = %v; want ErrNotFound", err)
		}
	})

	t.Run("purge of nonexistent keys is a no-op", func(t *testing.T) {
		c := newCache(t, 100, nil)
		if err := c.Purge([]string{"ghost"}, t.TempDir()); err != nil {
			t.Fatalf("Purge nonexistent: %v", err)
		}
	})
}

// --------------------------------------------------------------------------
// Pressure callback
// --------------------------------------------------------------------------

func TestPressureCallback(t *testing.T) {
	t.Run("callback fires when crossing 90%", func(t *testing.T) {
		var fired atomic.Int64
		fired90 := make(chan struct{}, 10) // buffered so goroutine never blocks

		onPressure := func() {
			fired.Add(1)
			select {
			case fired90 <- struct{}{}:
			default:
			}
		}

		// Limit = 100 bytes. 90 bytes → exactly 90% (not strictly over).
		// 91 bytes → 91*10=910 > 100*9=900 → fires.
		c := newCache(t, 100, onPressure)

		// Reserve 90 bytes: 90*10 = 900 == 900, NOT strictly greater → no fire.
		if !c.ReserveSpace(90) {
			t.Fatal("reserve 90 failed")
		}
		// Reserve 1 more byte: total 91, 91*10=910 > 900 → pressure fires.
		if !c.ReserveSpace(1) {
			t.Fatal("reserve 1 failed (total 91)")
		}
		// Wait for at least one pressure call.
		<-fired90

		if n := fired.Load(); n < 1 {
			t.Fatalf("pressure callback not fired; got %d fires", n)
		}
	})

	t.Run("callback does not fire below 90%", func(t *testing.T) {
		var fired atomic.Int64
		onPressure := func() { fired.Add(1) }

		c := newCache(t, 100, onPressure)
		// Reserve 89 bytes — 89*10 = 890 < 900, below threshold.
		if !c.ReserveSpace(89) {
			t.Fatal("reserve failed")
		}
		// Give any spurious goroutine a moment.
		// We deliberately avoid time.Sleep — if the goroutine fires, it fires
		// before or after, but the atomic check below is safe either way.
		c.FreeReserved(89) // clean up

		// This is inherently racy with a goroutine-based callback; we verify that
		// the condition (89*10 <= 900) was not met at the time of the call.
		// The implementation checks before launching the goroutine, so no fire
		// should have been scheduled.
		if n := fired.Load(); n != 0 {
			t.Fatalf("pressure callback fired unexpectedly (%d times) at 89%%", n)
		}
	})

	t.Run("nil OnPressure is a no-op", func(t *testing.T) {
		c := newCache(t, 10, nil)
		// Should not panic.
		if !c.ReserveSpace(10) {
			t.Fatal("reserve failed")
		}
	})
}

// --------------------------------------------------------------------------
// Limit = 0 (no-cache mode)
// --------------------------------------------------------------------------

func TestLimitZero(t *testing.T) {
	c := newCache(t, 0, nil)
	dir := t.TempDir()
	data := []byte("no-cache")

	if err := c.Save("z", dir, data); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if u := c.Used(); u != 0 {
		t.Fatalf("Used() = %d; want 0", u)
	}
	// Load should find the file on disk.
	got, err := c.Load("z", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Load = %q; want %q", got, data)
	}
}

// --------------------------------------------------------------------------
// Negative limit panics
// --------------------------------------------------------------------------

func TestNegativeLimitPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for negative Limit, got none")
		}
	}()
	New(Options{Limit: -1})
}

// --------------------------------------------------------------------------
// Concurrent access (race detector)
// --------------------------------------------------------------------------

func TestConcurrentAccess(t *testing.T) {
	const (
		goroutines   = 8
		articlesEach = 20
		articleSize  = 512
		limit        = goroutines * articlesEach * articleSize / 2 // half fit in mem
	)

	c := newCache(t, limit, nil)
	dir := t.TempDir()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < articlesEach; i++ {
				key := fmt.Sprintf("g%d-art%d", g, i)
				data := make([]byte, articleSize)
				for j := range data {
					data[j] = byte((g*articlesEach + i) % 256)
				}

				reserved := c.ReserveSpace(int64(len(data)))
				if err := c.Save(key, dir, data); err != nil {
					t.Errorf("Save %s: %v", key, err)
					if reserved {
						c.FreeReserved(int64(len(data)))
					}
					continue
				}
			}
		}()
	}
	wg.Wait()

	// Load all articles back and verify content.
	var totalLoaded int64
	var mu sync.Mutex
	var lwg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		lwg.Add(1)
		go func() {
			defer lwg.Done()
			for i := 0; i < articlesEach; i++ {
				key := fmt.Sprintf("g%d-art%d", g, i)
				data, err := c.Load(key, dir)
				if err != nil {
					t.Errorf("Load %s: %v", key, err)
					continue
				}
				mu.Lock()
				totalLoaded += int64(len(data))
				mu.Unlock()
			}
		}()
	}
	lwg.Wait()

	wantTotal := int64(goroutines * articlesEach * articleSize)
	if totalLoaded != wantTotal {
		t.Fatalf("totalLoaded = %d; want %d", totalLoaded, wantTotal)
	}

	// After all loads, Used() must be 0.
	if u := c.Used(); u != 0 {
		t.Fatalf("Used() = %d after all Loads; want 0", u)
	}
}

// --------------------------------------------------------------------------
// diskPath helper (internal)
// --------------------------------------------------------------------------

func TestDiskPathIsDeterministic(t *testing.T) {
	p1 := diskPath("/tmp/admin", "test-message-id@host")
	p2 := diskPath("/tmp/admin", "test-message-id@host")
	if p1 != p2 {
		t.Fatalf("diskPath not deterministic: %q vs %q", p1, p2)
	}
	// Different keys must produce different paths.
	p3 := diskPath("/tmp/admin", "other-id@host")
	if p1 == p3 {
		t.Fatal("different keys produced same disk path")
	}
	// Path must be under adminDir.
	expected := filepath.Join("/tmp/admin", "")
	if len(p1) <= len(expected) || p1[:len(expected)] != expected {
		t.Fatalf("path %q is not under adminDir /tmp/admin", p1)
	}
}
