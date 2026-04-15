// Package cache implements a memory-bounded article cache with per-job disk
// spill. Articles held in memory are removed and returned on Load; articles
// that would exceed the memory limit are written to disk under the job's admin
// directory and faulted in on Load.
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
	articles map[string]cachedEntry // key → in-memory entry
	used     int64                  // bytes currently charged (reserved + stored)

	// usedAtomic mirrors used and is updated under mu. It allows Used() to be
	// read lock-free by callers that only need an approximate snapshot.
	usedAtomic atomic.Int64

	onPressure func() // may be nil
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

// ReserveSpace attempts to reserve size bytes from the memory budget. Returns
// true on success; the caller must follow up with Save (passing data of length
// size) or FreeReserved(size) if Save will not happen.
//
// ReserveSpace pre-charges c.used by size. Save detects this charge and avoids
// double-counting.
func (c *Cache) ReserveSpace(size int64) bool {
	if c.limit == 0 {
		// No-cache mode: every article goes to disk, nothing to reserve.
		return false
	}
	c.mu.Lock()
	newUsed := c.used + size
	if newUsed > c.limit {
		c.mu.Unlock()
		return false
	}
	c.used = newUsed
	c.usedAtomic.Store(newUsed)
	c.mu.Unlock()

	c.maybePressure(newUsed)
	return true
}

// FreeReserved releases size bytes previously acquired by ReserveSpace but not
// followed by a corresponding Save.
func (c *Cache) FreeReserved(size int64) {
	c.mu.Lock()
	c.used -= size
	if c.used < 0 {
		c.used = 0
	}
	c.usedAtomic.Store(c.used)
	c.mu.Unlock()
}

// Save stores data for key. If the caller previously called
// ReserveSpace(len(data)) successfully the article is kept in memory.
// Otherwise it is written to {adminDir}/{sha256(key)}.
//
// Counter accounting: ReserveSpace pre-charges c.used by len(data). Save must
// not charge again. It detects a valid reservation by checking that c.used
// already accounts for len(data) bytes beyond any existing entry for the same
// key (c.used − oldEntrySize >= len(data) and c.used <= limit).
//
// Idempotent replace: if the key is already in memory, the old entry's bytes
// are subtracted from c.used. The caller's ReserveSpace call for the new size
// has already been charged, so net c.used = (c.used − oldSize) stays correct.
func (c *Cache) Save(key, adminDir string, data []byte) error {
	newSize := int64(len(data))

	c.mu.Lock()

	oldSize := int64(0)
	if existing, ok := c.articles[key]; ok {
		oldSize = int64(len(existing.data))
	}

	// A valid reservation is in effect when:
	//   - limit > 0 (memory cache is active)
	//   - c.used <= c.limit (the reservation did not overflow the budget)
	//   - (c.used - oldSize) >= newSize (the reservation covers the new data,
	//     after accounting for the old entry that will be replaced)
	//
	// The third condition is the key invariant: ReserveSpace charged newSize
	// into c.used. If an old entry for the same key exists, its bytes are still
	// counted in c.used too, so we subtract them to isolate the reservation.
	hasReservation := c.limit > 0 && c.used <= c.limit && (c.used-oldSize) >= newSize

	if hasReservation {
		// Move old entry size out of the counter; newSize was already charged by
		// ReserveSpace, so the net effect is c.used = c.used - oldSize.
		if oldSize > 0 {
			delete(c.articles, key)
			c.used -= oldSize
		}
		c.articles[key] = cachedEntry{data: data, adminDir: adminDir}
		c.usedAtomic.Store(c.used)
		used := c.used
		c.mu.Unlock()
		c.maybePressure(used)
		return nil
	}

	// No reservation in effect (or limit == 0). Spill to disk.
	// If an old in-memory entry is being overwritten via a disk-spill Save,
	// remove it and release its counter charge.
	if oldSize > 0 {
		delete(c.articles, key)
		c.used -= oldSize
		if c.used < 0 {
			c.used = 0
		}
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

	// Try disk.
	path := diskPath(adminDir, key)
	// Path is constructed by diskPath as filepath.Join(adminDir, sha256hex);
	// adminDir is a trusted caller-supplied directory, not external user input.
	//nolint:gosec // G304: path is under caller-controlled adminDir with sha256 filename
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("cache: load %s: %w", key, err)
	}
	// Remove the file — Load consumes the entry.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cache: remove after load %s: %w", key, err)
	}
	return data, nil
}

// Flush writes all in-memory entries to disk and empties the memory map. Called
// on shutdown or when direct_write is toggled.
func (c *Cache) Flush() error {
	c.mu.Lock()
	// Snapshot and clear atomically so Load/Save can proceed immediately.
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

// Used returns the current in-memory byte count (reserved + stored). The value
// is an atomic snapshot and may lag slightly behind the mutex-protected c.used.
func (c *Cache) Used() int64 {
	return c.usedAtomic.Load()
}

// Limit returns the configured memory limit.
func (c *Cache) Limit() int64 {
	return c.limit
}

// maybePressure fires OnPressure in a goroutine if used > 90% of limit.
// Must be called without mu held to avoid holding the lock across a goroutine
// launch.
func (c *Cache) maybePressure(used int64) {
	if c.onPressure == nil || c.limit == 0 {
		return
	}
	// Integer arithmetic: used*10 > limit*9  ⟺  used/limit > 0.9.
	if used*10 > c.limit*9 {
		go c.onPressure()
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
	return filepath.Join(adminDir, fmt.Sprintf("%x", sum))
}
