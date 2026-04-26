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

	// Won't fit. Drop any existing in-memory entry so Load will find the new
	// disk copy rather than stale memory, then spill to disk.
	if oldSize > 0 {
		delete(c.articles, key)
		c.used -= oldSize
		c.usedAtomic.Store(c.used)
	}
	c.mu.Unlock()

	return c.writeToDisk(key, adminDir, data)
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
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cache: remove after load %s: %w", key, err)
	}
	return data, nil
}

// Flush writes all in-memory entries to disk and empties the memory map. Called
// on shutdown or when direct_write is toggled.
func (c *Cache) Flush() error {
	c.mu.Lock()
	snapshot := c.articles
	c.articles = make(map[string]cachedEntry)
	c.used = 0
	c.usedAtomic.Store(0)
	c.mu.Unlock()

	var firstErr error
	for key, entry := range snapshot {
		if err := c.writeToDisk(key, entry.adminDir, entry.data); err != nil && firstErr == nil {
			firstErr = err
		}
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

// writeToDisk persists data to {adminDir}/{sha256(key)}.
func (c *Cache) writeToDisk(key, adminDir string, data []byte) error {
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		return fmt.Errorf("cache: mkdir %s: %w", adminDir, err)
	}
	path := diskPath(adminDir, key)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("cache: write %s: %w", key, err)
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
