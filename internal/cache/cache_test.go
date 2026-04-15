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

func newCache(t *testing.T, limit int64, onPressure func()) *Cache {
	t.Helper()
	return New(Options{Limit: limit, OnPressure: onPressure})
}

// --------------------------------------------------------------------------
// CanFit
// --------------------------------------------------------------------------

func TestCanFit(t *testing.T) {
	t.Run("within limit returns true", func(t *testing.T) {
		c := newCache(t, 100, nil)
		if !c.CanFit(50) {
			t.Fatal("CanFit(50) with empty 100-byte cache should be true")
		}
	})

	t.Run("exceeds limit returns false", func(t *testing.T) {
		c := newCache(t, 100, nil)
		if c.CanFit(101) {
			t.Fatal("CanFit(101) with limit 100 should be false")
		}
	})

	t.Run("reflects occupancy after Save", func(t *testing.T) {
		c := newCache(t, 100, nil)
		if err := c.Save("k", t.TempDir(), make([]byte, 60)); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if !c.CanFit(40) {
			t.Fatal("CanFit(40) after 60 stored with limit 100 should be true")
		}
		if c.CanFit(41) {
			t.Fatal("CanFit(41) after 60 stored with limit 100 should be false")
		}
	})

	t.Run("limit 0 always returns false", func(t *testing.T) {
		c := newCache(t, 0, nil)
		if c.CanFit(1) {
			t.Fatal("CanFit on limit=0 cache should be false")
		}
	})

	t.Run("makes no reservation", func(t *testing.T) {
		c := newCache(t, 100, nil)
		_ = c.CanFit(50)
		_ = c.CanFit(50)
		if got := c.Used(); got != 0 {
			t.Fatalf("CanFit reserved space (Used=%d); expected 0", got)
		}
	})
}

// --------------------------------------------------------------------------
// Save — memory path
// --------------------------------------------------------------------------

func TestSaveMemory(t *testing.T) {
	t.Run("save within limit stays in memory", func(t *testing.T) {
		c := newCache(t, 200, nil)
		dir := t.TempDir()
		data := []byte("hello article")
		if err := c.Save("msg1", dir, data); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if got := c.Used(); got != int64(len(data)) {
			t.Fatalf("Used() = %d; want %d", got, len(data))
		}
		// File should NOT be on disk — it fit in memory.
		if _, err := os.Stat(diskPath(dir, "msg1")); !os.IsNotExist(err) {
			t.Fatalf("unexpected disk file for in-memory entry: stat err = %v", err)
		}
	})

	t.Run("idempotent save replaces entry and adjusts counter", func(t *testing.T) {
		c := newCache(t, 200, nil)
		dir := t.TempDir()
		data1 := []byte("first version")
		data2 := []byte("second longer version")

		if err := c.Save("msg-idem", dir, data1); err != nil {
			t.Fatalf("first Save: %v", err)
		}
		if err := c.Save("msg-idem", dir, data2); err != nil {
			t.Fatalf("second Save: %v", err)
		}
		if got := c.Used(); got != int64(len(data2)) {
			t.Fatalf("Used() = %d; want %d", got, len(data2))
		}
	})

	t.Run("replace-to-smaller frees space correctly", func(t *testing.T) {
		c := newCache(t, 200, nil)
		dir := t.TempDir()
		if err := c.Save("k", dir, make([]byte, 150)); err != nil {
			t.Fatalf("Save 150: %v", err)
		}
		if err := c.Save("k", dir, make([]byte, 50)); err != nil {
			t.Fatalf("Save 50: %v", err)
		}
		if got := c.Used(); got != 50 {
			t.Fatalf("Used() = %d; want 50", got)
		}
	})
}

// --------------------------------------------------------------------------
// Save — disk spill path
// --------------------------------------------------------------------------

func TestSaveDisk(t *testing.T) {
	t.Run("save that does not fit spills to disk", func(t *testing.T) {
		c := newCache(t, 100, nil)
		dir := t.TempDir()
		// Fill memory to 80/100.
		if err := c.Save("filler", dir, make([]byte, 80)); err != nil {
			t.Fatalf("Save filler: %v", err)
		}
		// Now save 50 more — does not fit; must spill.
		spill := []byte("spilled article over limit")
		if err := c.Save("spill-key", dir, spill); err != nil {
			t.Fatalf("Save spill: %v", err)
		}
		// Memory must still show only the filler.
		if got := c.Used(); got != 80 {
			t.Fatalf("Used() = %d; want 80", got)
		}
		if _, err := os.Stat(diskPath(dir, "spill-key")); err != nil {
			t.Fatalf("expected spill file at disk: %v", err)
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
		if _, err := os.Stat(diskPath(dir, "nc-key")); err != nil {
			t.Fatalf("expected disk file: %v", err)
		}
	})

	t.Run("replace in-memory with oversized spills and evicts old entry", func(t *testing.T) {
		c := newCache(t, 100, nil)
		dir := t.TempDir()
		if err := c.Save("k", dir, make([]byte, 50)); err != nil {
			t.Fatalf("first Save: %v", err)
		}
		// 200-byte replacement won't fit — must spill and evict the old memory entry.
		big := make([]byte, 200)
		for i := range big {
			big[i] = 0xab
		}
		if err := c.Save("k", dir, big); err != nil {
			t.Fatalf("oversize Save: %v", err)
		}
		if got := c.Used(); got != 0 {
			t.Fatalf("Used() = %d; want 0 after eviction", got)
		}
		// Load must return the new (disk) copy, not the old (memory) one.
		got, err := c.Load("k", dir)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(got) != 200 || got[0] != 0xab {
			t.Fatalf("Load returned stale data: len=%d first=%x", len(got), got[0])
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
		if u := c.Used(); u != 0 {
			t.Fatalf("Used() = %d after Load; want 0", u)
		}
		if _, err := c.Load("load-mem", dir); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second Load error = %v; want ErrNotFound", err)
		}
	})

	t.Run("load from disk after spill consumes file", func(t *testing.T) {
		c := newCache(t, 0, nil) // no-cache → always disk
		dir := t.TempDir()
		data := []byte("disk article")
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
		path := diskPath(dir, "disk-key")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected file removed after Load, stat err = %v", err)
		}
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
	c := newCache(t, 1024, nil)
	dir := t.TempDir()
	articles := map[string][]byte{
		"art1": []byte("data one"),
		"art2": []byte("data two"),
		"art3": []byte("data three"),
	}
	for key, data := range articles {
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
	for key, want := range articles {
		got, err := c.Load(key, dir)
		if err != nil {
			t.Fatalf("Load after flush %s: %v", key, err)
		}
		if string(got) != string(want) {
			t.Fatalf("Load after flush %s = %q; want %q", key, got, want)
		}
	}
}

// --------------------------------------------------------------------------
// Purge
// --------------------------------------------------------------------------

func TestPurge(t *testing.T) {
	t.Run("purge removes from memory and disk", func(t *testing.T) {
		c := newCache(t, 100, nil)
		dir := t.TempDir()

		memData := []byte("in memory")
		diskData := make([]byte, 150) // won't fit in 100-byte cache → spills

		if err := c.Save("memKey", dir, memData); err != nil {
			t.Fatalf("Save memKey: %v", err)
		}
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
		signal := make(chan struct{}, 10)

		onPressure := func() {
			fired.Add(1)
			select {
			case signal <- struct{}{}:
			default:
			}
		}

		// Limit 100: save 90 (at exactly 90%, no fire), then save 1 more (91%, fires).
		c := newCache(t, 100, onPressure)
		dir := t.TempDir()

		if err := c.Save("a", dir, make([]byte, 90)); err != nil {
			t.Fatalf("Save 90: %v", err)
		}
		if err := c.Save("b", dir, make([]byte, 1)); err != nil {
			t.Fatalf("Save 1: %v", err)
		}
		<-signal
		if n := fired.Load(); n < 1 {
			t.Fatalf("pressure callback not fired; got %d fires", n)
		}
	})

	t.Run("callback does not fire below 90%", func(t *testing.T) {
		var fired atomic.Int64
		onPressure := func() { fired.Add(1) }

		c := newCache(t, 100, onPressure)
		if err := c.Save("a", t.TempDir(), make([]byte, 89)); err != nil {
			t.Fatalf("Save: %v", err)
		}
		// maybePressure inspects the threshold before launching the goroutine, so
		// if the condition is false no goroutine is scheduled. We can read fired
		// directly without waiting.
		if n := fired.Load(); n != 0 {
			t.Fatalf("pressure callback fired unexpectedly (%d times) at 89%%", n)
		}
	})

	t.Run("nil OnPressure is a no-op", func(t *testing.T) {
		c := newCache(t, 10, nil)
		if err := c.Save("x", t.TempDir(), make([]byte, 10)); err != nil {
			t.Fatalf("Save: %v", err)
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
		go func(g int) {
			defer wg.Done()
			for i := 0; i < articlesEach; i++ {
				key := fmt.Sprintf("g%d-art%d", g, i)
				data := make([]byte, articleSize)
				for j := range data {
					data[j] = byte((g*articlesEach + i) % 256)
				}
				if err := c.Save(key, dir, data); err != nil {
					t.Errorf("Save %s: %v", key, err)
				}
			}
		}(g)
	}
	wg.Wait()

	var totalLoaded int64
	var mu sync.Mutex
	var lwg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		lwg.Add(1)
		go func(g int) {
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
		}(g)
	}
	lwg.Wait()

	wantTotal := int64(goroutines * articlesEach * articleSize)
	if totalLoaded != wantTotal {
		t.Fatalf("totalLoaded = %d; want %d", totalLoaded, wantTotal)
	}
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
	p3 := diskPath("/tmp/admin", "other-id@host")
	if p1 == p3 {
		t.Fatal("different keys produced same disk path")
	}
	expected := filepath.Join("/tmp/admin", "")
	if len(p1) <= len(expected) || p1[:len(expected)] != expected {
		t.Fatalf("path %q is not under adminDir /tmp/admin", p1)
	}
}
