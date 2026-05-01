// Package dirscanner provides directory scanning for NZB files and archives,
// detecting stable files across scans and extracting them for processing.
package dirscanner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileState tracks a file's observed size and modification time to detect stability.
type FileState struct {
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// Store manages the persistence of file states to a JSON file.
type Store struct {
	mu     sync.RWMutex
	path   string
	states map[string]FileState
	dirty  bool
}

// OpenStore opens or creates a JSON state file at the given path. If the file
// exists and is valid JSON, prior state is loaded; otherwise a fresh map is used.
func OpenStore(path string) (*Store, error) {
	store := &Store{
		path:   path,
		states: make(map[string]FileState),
	}

	// Potential file inclusion is expected: we're reading from a known path
	//nolint:gosec // G304: opening path provided by caller/config
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to read state file: %w", err)
		}
	} else {
		if err := json.Unmarshal(data, &store.states); err != nil {
			return nil, fmt.Errorf("failed to parse state file: %w", err)
		}
	}

	return store, nil
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
	s.dirty = true
}

// Delete removes the state entry for the given path.
func (s *Store) Delete(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.states[path]; ok {
		delete(s.states, path)
		s.dirty = true
	}
}

// Save persists all states to the JSON file. Uses atomic writes (temp + rename).
// If no state has changed since the last Save (or since load), this is a no-op.
func (s *Store) Save() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	s.dirty = false
	data, err := json.MarshalIndent(s.states, "", "  ")
	s.mu.Unlock()

	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(s.path), ".dirscanner-*.tmp")
	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return fmt.Errorf("failed to create temporary state file: %w", err)
	}
	tmpName := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return fmt.Errorf("failed to write temporary state file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpName)
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return fmt.Errorf("failed to close temporary state file: %w", err)
	}

	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // cleanup of temp file
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return fmt.Errorf("failed to rename state file: %w", err)
	}

	return nil
}
