// Package cache implements a memory-bounded article cache with per-job disk
// spill. Articles held in memory are removed and returned on Load; articles
// that would exceed the memory limit are written to disk under the job's admin
// directory and faulted in on Load.
//
// Callers do not pre-reserve space. Save is authoritative: it keeps the
// article in memory if the limit allows, otherwise spills to disk. CanFit is
// an advisory predicate for admission control (e.g., deciding whether to
// accept a new job given its expected total size); it makes no reservation.
//
// Key design: sha256 is used to derive disk filenames from Message-IDs rather
// than hex-encoding the raw bytes. Both approaches are collision-free, but
// sha256 gives a fixed 64-character hex name regardless of Message-ID length,
// and it guards against pathological inputs that contain filesystem-unsafe
// characters (slashes, nulls, colons on Windows, etc.). The raw-hex
// alternative would be reversible but can be arbitrarily long and include
// unsafe characters verbatim.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// ErrNotFound is returned by Load when the key is absent from both memory
// and disk.
var ErrNotFound = errors.New("cache: article not found")

// Options configures a Cache.
type Options struct {
	// Limit is the maximum bytes held in memory. Writes that would exceed Limit
	// spill to disk. A Limit of 0 means "no cache" — every Save goes straight
	// to disk. Negative values panic.
	Limit int64

	// OnPressure is invoked in a goroutine when in-memory usage crosses 90% of
	// Limit. The callback may be invoked many times in quick succession; callers
	// are responsible for their own coalescing.
	OnPressure func()
}

// cachedEntry holds an article kept in memory together with the admin directory
// it belongs to, so Flush knows where to spill it.
type cachedEntry struct {
	data     []byte
	adminDir string
}

// Cache is a memory-bounded article store. Zero value is not usable; create
// with New.
type Cache struct {
	limit int64 // immutable after New

	mu       sync.Mutex
	articles map[string]cachedEntry
	used     int64

	// usedAtomic mirrors used and is updated under mu. It allows Used() and
	// CanFit() to be read lock-free.
	usedAtomic atomic.Int64

	onPressure     func() // may be nil
	pressureActive atomic.Bool
}

// New creates a Cache from opts. It panics if opts.Limit is negative.
func New(opts Options) *Cache {
	if opts.Limit < 0 {
		panic("cache: Options.Limit must be >= 0")
	}
	return &Cache{
		limit:      opts.Limit,
		articles:   make(map[string]cachedEntry),
		onPressure: opts.OnPressure,
	}
}

// CanFit reports whether size bytes would currently fit in the memory budget.
// The value is advisory — no reservation is made and the answer may change
// before a subsequent Save runs. Returns false when Limit is 0 (no-cache mode).
func (c *Cache) CanFit(size int64) bool {
	if c.limit == 0 {
		return false
	}
	return c.usedAtomic.Load()+size <= c.limit
}

// Save stores data for key. If the memory budget allows, the article is kept
// in memory; otherwise it is written to {adminDir}/{sha256(key)}. Saving a
// key already in memory replaces the existing entry and adjusts the counter.
// Saving over an in-memory entry with data that no longer fits evicts the old
// entry and spills the new one to disk.
func (c *Cache) Save(key, adminDir string, data []byte) error {
	newSize := int64(len(data))

	c.mu.Lock()
	oldSize := int64(0)
	if existing, ok := c.articles[key]; ok {
		oldSize = int64(len(existing.data))
	}
	newUsed := c.used - oldSize + newSize

	if c.limit > 0 && newUsed <= c.limit {
		c.articles[key] = cachedEntry{data: data, adminDir: adminDir}
		c.used = newUsed
		c.usedAtomic.Store(newUsed)
		c.mu.Unlock()
		c.maybePressure(newUsed)
		return nil
	}

	// Won't fit in memory. Write to disk first, then remove the old
	// in-memory entry. This ordering prevents a window where the article
	// is neither in memory nor on disk (which would cause concurrent
	// Load to return ErrNotFound).
	c.mu.Unlock()

	if err := c.writeToDisk(key, adminDir, data); err != nil {
		return err
	}

	// Now that the disk copy is durable, remove any stale memory entry.
	if oldSize > 0 {
		c.mu.Lock()
		// Re-check: a concurrent Save might have placed a newer entry.
		if cur, ok := c.articles[key]; ok && len(cur.data) == int(oldSize) {
			delete(c.articles, key)
			c.used -= oldSize
			c.usedAtomic.Store(c.used)
		}
		c.mu.Unlock()
	}
	return nil
}

// Load retrieves an article. Memory is checked first; on miss the disk copy at
// {adminDir}/{sha256(key)} is tried. On a hit the article is consumed
// (removed from memory or disk). Returns ErrNotFound if neither location has
// the key.
func (c *Cache) Load(key, adminDir string) ([]byte, error) {
	c.mu.Lock()
	if entry, ok := c.articles[key]; ok {
		delete(c.articles, key)
		c.used -= int64(len(entry.data))
		if c.used < 0 {
			c.used = 0
		}
		c.usedAtomic.Store(c.used)
		c.mu.Unlock()
		return entry.data, nil
	}
	c.mu.Unlock()

	path := diskPath(adminDir, key)
	// adminDir is a trusted caller-supplied directory; the filename component
	// is a fixed-length sha256 hex string produced by diskPath.
	//nolint:gosec // G304: path is under caller-controlled adminDir with sha256 filename
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("cache: load %s: %w", key, err)
	}
	// Best-effort cleanup — the data was successfully read, so don't
	// fail the load if removal fails (e.g. transient filesystem lock).
	_ = os.Remove(path) //nolint:errcheck
	return data, nil
}

// Flush writes all in-memory entries to disk and empties the memory map. Called
// on shutdown or when direct_write is toggled.
//
// Entries remain visible in memory until their disk write completes, so
// concurrent Load calls never see an invisible window.
func (c *Cache) Flush() error {
	// Snapshot the keys under the lock, but leave the data in the map.
	c.mu.Lock()
	keys := make([]string, 0, len(c.articles))
	for k := range c.articles {
		keys = append(keys, k)
	}
	c.mu.Unlock()

	var firstErr error
	for _, key := range keys {
		c.mu.Lock()
		entry, ok := c.articles[key]
		if !ok {
			// A concurrent Load consumed it — nothing to flush.
			c.mu.Unlock()
			continue
		}
		c.mu.Unlock()

		if err := c.writeToDisk(key, entry.adminDir, entry.data); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue // Leave in memory so data isn't lost.
		}

		// Remove from memory only after successful disk write. Guard
		// against a concurrent Save that replaced the entry with newer
		// data while we were writing: only delete if the data length
		// still matches what we wrote.
		c.mu.Lock()
		cur, exists := c.articles[key]
		switch {
		case exists && len(cur.data) > 0 && len(entry.data) > 0 && &cur.data[0] == &entry.data[0]:
			// Entry unchanged (same backing array) — consume it.
			delete(c.articles, key)
			c.used -= int64(len(entry.data))
			if c.used < 0 {
				c.used = 0
			}
			c.usedAtomic.Store(c.used)
		case !exists:
			// A concurrent Load consumed the entry from memory while we
			// were writing to disk. Clean up the now-orphaned disk file.
			_ = os.Remove(diskPath(entry.adminDir, key))
		}
		c.mu.Unlock()
	}
	return firstErr
}

// Purge removes the given keys from memory and disk. Used when a job is
// cancelled.
func (c *Cache) Purge(keys []string, adminDir string) error {
	c.mu.Lock()
	for _, key := range keys {
		if entry, ok := c.articles[key]; ok {
			c.used -= int64(len(entry.data))
			if c.used < 0 {
				c.used = 0
			}
			delete(c.articles, key)
		}
	}
	c.usedAtomic.Store(c.used)
	c.mu.Unlock()

	var firstErr error
	for _, key := range keys {
		path := diskPath(adminDir, key)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = fmt.Errorf("cache: purge remove %s: %w", key, err)
			}
		}
	}
	return firstErr
}

// Used returns the current in-memory byte count.
func (c *Cache) Used() int64 {
	return c.usedAtomic.Load()
}

// Limit returns the configured memory limit.
func (c *Cache) Limit() int64 {
	return c.limit
}

// maybePressure fires OnPressure in a goroutine if used > 90% of limit.
// Uses an atomic flag to coalesce: at most one goroutine runs at a time.
// Must be called without mu held.
func (c *Cache) maybePressure(used int64) {
	if c.onPressure == nil || c.limit == 0 {
		return
	}
	// Integer arithmetic: used*10 > limit*9  ⟺  used/limit > 0.9.
	if used*10 > c.limit*9 {
		if c.pressureActive.CompareAndSwap(false, true) {
			go func() {
				defer c.pressureActive.Store(false)
				c.onPressure()
			}()
		}
	}
}

// writeToDisk persists data to {adminDir}/{sha256(key)} atomically.
// Writes to a temporary file first, then renames, so concurrent Load
// never observes a partial file.
func (c *Cache) writeToDisk(key, adminDir string, data []byte) error {
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		return fmt.Errorf("cache: mkdir %s: %w", adminDir, err)
	}
	path := diskPath(adminDir, key)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("cache: write %s: %w", key, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) // clean up on rename failure
		return fmt.Errorf("cache: rename %s: %w", key, err)
	}
	return nil
}

// diskPath returns the filesystem path for a key under adminDir. sha256 is
// used so that the resulting filename is always 64 hex characters regardless
// of the Message-ID length, and so that filesystem-unsafe characters in the
// raw Message-ID (slashes, nulls, colons) cannot escape the admin directory.
func diskPath(adminDir, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(adminDir, hex.EncodeToString(sum[:]))
}
