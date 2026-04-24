package dirscanner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/types"
)

// Handler defines the interface for consuming NZB payloads extracted by the scanner.
// It receives the original filename and the decompressed NZB data.
type Handler interface {
	HandleNZB(ctx context.Context, filename string, data []byte, opts types.FetchOptions) error
}

// Scanner watches a directory for stable NZB files and decompressed archives,
// extracts them, and passes them to a handler.
type Scanner struct {
	dir     string
	store   *Store
	handler Handler
	logger  *slog.Logger
}

// New creates a new Scanner for the given directory.
func New(dir string, store *Store, h Handler, logger *slog.Logger) *Scanner {
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "dirscanner")
	return &Scanner{
		dir:     dir,
		store:   store,
		handler: h,
		logger:  log,
	}
}

// ScanOnce performs one scan of the directory. It detects stable files (matching
// size and mtime from a prior scan), decompresses them, invokes the handler,
// and on success deletes the source file. Returns the number of files processed.
func (s *Scanner) ScanOnce(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("failed to read directory: %w", err)
	}

	processed := 0

	// Current scan state: map of path -> FileState for files we see now.
	currentScan := make(map[string]FileState)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		path := filepath.Join(s.dir, entry.Name())

		// Skip dotfiles and non-matching extensions.
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		if !isValidExtension(entry.Name()) {
			continue
		}

		// Get current file stat.
		stat, err := os.Stat(path)
		if err != nil {
			s.logger.Warn("failed to stat file", "path", path, "err", err)
			continue
		}

		currentScan[path] = FileState{
			Size:  stat.Size(),
			MTime: stat.ModTime(),
		}

		// Check if this file was seen in a prior scan with the same state.
		priorState, wasSeen := s.store.Get(path)
		if !wasSeen {
			// First sighting: record it and move to next file.
			s.store.Set(path, currentScan[path])
			s.logger.Debug("first sighting of file", "path", path)
			continue
		}

		// File was seen before. Check if it's stable (same size+mtime).
		if priorState.Size != currentScan[path].Size || priorState.MTime != currentScan[path].MTime {
			// File changed: reset the stable timer by updating its recorded state.
			s.store.Set(path, currentScan[path])
			s.logger.Debug("file changed, resetting stability timer", "path", path)
			continue
		}

		// File is stable. Extract, handle, and clean up.
		if err := s.handleStableFile(ctx, path, entry.Name()); err != nil {
			s.logger.Warn("failed to handle file", "path", path, "err", err)
			continue
		}

		processed++
		// Don't delete the entry from store; it may be helpful for later audits.
	}

	// Remove entries from store that no longer exist on disk.
	var toDelete []string
	for storedPath := range make(map[string]bool) {
		// Merge all stored paths.
		if _, exists := currentScan[storedPath]; !exists {
			toDelete = append(toDelete, storedPath)
		}
	}

	// Actually iterate over store state.
	s.store.mu.RLock()
	for storedPath := range s.store.states {
		if _, exists := currentScan[storedPath]; !exists {
			toDelete = append(toDelete, storedPath)
		}
	}
	s.store.mu.RUnlock()

	for _, path := range toDelete {
		s.store.Delete(path)
	}

	// Persist updated state to disk.
	if err := s.store.Save(); err != nil {
		s.logger.Warn("failed to save state", "err", err)
	}

	return processed, nil
}

// handleStableFile extracts NZBs from a stable file and invokes the handler.
func (s *Scanner) handleStableFile(ctx context.Context, path, filename string) error {
	nzbs, err := ExtractNZBs(path)
	if err != nil {
		return fmt.Errorf("failed to extract NZBs: %w", err)
	}

	// Invoke handler for each NZB. If any fails, log but continue with the rest.
	var lastErr error
	successCount := 0

	for i, nzbData := range nzbs {
		// For archives with multiple NZBs, label them.
		label := filename
		if len(nzbs) > 1 {
			label = fmt.Sprintf("%s[%d]", filename, i+1)
		}

		if err := s.handler.HandleNZB(ctx, label, nzbData, types.FetchOptions{}); err != nil {
			s.logger.Warn("handler failed for NZB", "label", label, "err", err)
			lastErr = err
			continue
		}

		successCount++
	}

	// If at least one succeeded, delete the file. Otherwise, leave it for retry.
	if successCount > 0 {
		if err := os.Remove(path); err != nil {
			s.logger.Warn("failed to delete file after successful handling", "path", path, "err", err)
		}
		return nil
	}

	// All handlers failed.
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no NZBs processed")
}

// Run starts a long-lived loop that scans the directory at regular intervals
// until the context is cancelled.
func (s *Scanner) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			count, err := s.ScanOnce(ctx)
			if err != nil {
				s.logger.Warn("scan failed", "err", err)
				continue
			}
			if count > 0 {
				s.logger.Info("scan complete", "processed", count)
			}
		}
	}
}

// isValidExtension checks if a filename has a valid NZB or archive extension.
func isValidExtension(filename string) bool {
	lower := strings.ToLower(filename)
	return strings.HasSuffix(lower, ".nzb") ||
		strings.HasSuffix(lower, ".nzb.gz") ||
		strings.HasSuffix(lower, ".nzb.bz2") ||
		strings.HasSuffix(lower, ".zip") ||
		strings.HasSuffix(lower, ".rar")
}
