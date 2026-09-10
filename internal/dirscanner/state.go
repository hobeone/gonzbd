// Package dirscanner provides directory scanning for NZB files and archives,
// detecting stable files across scans and extracting them for processing.
package dirscanner

import (
	"sync"
	"time"
)

// FileState tracks a file's observed size and modification time to detect stability.
type FileState struct {
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// Store tracks observed file states in memory for stability detection
// (has this path been seen before with the same size+mtime). It is not
// persisted: losing it across a restart costs at most one extra scan
// interval before a file that was mid-transfer at restart time is
// considered stable again, which is not worth the complexity or the
// disk-state-leak surface a file-backed store carries — see
// isGoneFromScannedDir's category-removal case, which this store no
// longer has any exposure to once nothing is written to disk.
type Store struct {
	mu     sync.RWMutex
	states map[string]FileState
}

// NewStore returns a fresh, empty in-memory Store.
func NewStore() *Store {
	return &Store{states: make(map[string]FileState)}
}

// Get retrieves the state for a file path. Returns false if not found.
func (s *Store) Get(path string) (FileState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.states[path]
	return state, ok
}

// Set updates or creates the state for a file path.
func (s *Store) Set(path string, state FileState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[path] = state
}

// Delete removes the state entry for the given path.
func (s *Store) Delete(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, path)
}
