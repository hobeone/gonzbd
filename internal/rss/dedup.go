package rss

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Store tracks item IDs that have been seen so the scanner does not dispatch
// the same item twice. IDs are persisted to a JSON file between process restarts.
type Store struct {
	mu   sync.Mutex
	path string
	seen map[string]time.Time // ID → first-seen UTC timestamp
}

// storeFile is the on-disk representation.
type storeFile struct {
	Seen map[string]time.Time `json:"seen"`
}

// OpenStore loads an existing store from path, creating a new one if the file
// does not exist. Any other I/O error is returned to the caller.
func OpenStore(path string) (*Store, error) {
	s := &Store{
		path: path,
		seen: make(map[string]time.Time),
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: path is caller-supplied config; not user HTTP input
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("rss: open store %q: %w", path, err)
	}

	var f storeFile
	if err = json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("rss: decode store %q: %w", path, err)
	}
	if f.Seen != nil {
		s.seen = f.Seen
	}
	return s, nil
}

// Seen reports whether id has already been recorded.
func (s *Store) Seen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.seen[id]
	return ok
}

// Record marks id as seen at the current UTC time.
// Calling Record on an already-seen id is a no-op.
func (s *Store) Record(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[id]; !ok {
		s.seen[id] = time.Now().UTC()
	}
}

// Save flushes the current state to disk atomically (write-then-rename).
func (s *Store) Save() error {
	s.mu.Lock()
	snapshot := make(map[string]time.Time, len(s.seen))
	for k, v := range s.seen {
		snapshot[k] = v
	}
	s.mu.Unlock()

	data, err := json.Marshal(storeFile{Seen: snapshot})
	if err != nil {
		return fmt.Errorf("rss: encode store: %w", err)
	}

	tmp := s.path + ".tmp"
	//nolint:gosec // G306: config/state file; group+world read is intentional
	if err = os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("rss: write store tmp: %w", err)
	}
	if err = os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rss: rename store: %w", err)
	}
	return nil
}

// Prune removes entries older than the given duration and returns the count
// of entries removed.
func (s *Store) Prune(older time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().UTC().Add(-older)
	removed := 0
	for id, ts := range s.seen {
		if ts.Before(cutoff) {
			delete(s.seen, id)
			removed++
		}
	}
	return removed
}
