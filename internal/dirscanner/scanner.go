package dirscanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/types"
)

// PartialError indicates that some but not all NZBs from an archive were
// successfully imported. The scanner uses this to decide whether to leave
// the source file for retry (partial success) or rename it to .failed
// (total failure).
type PartialError struct {
	Failed int
	Total  int
	Err    error
}

func (e *PartialError) Error() string {
	return fmt.Sprintf("partial: %d of %d NZBs failed: %v", e.Failed, e.Total, e.Err)
}

func (e *PartialError) Unwrap() error { return e.Err }

// Handler defines the interface for consuming NZB payloads extracted by the scanner.
// It receives the original filename and the decompressed NZB data.
// Returns the created job ID (or empty string) and an error.
type Handler interface {
	HandleNZB(ctx context.Context, filename string, data []byte, opts types.FetchOptions) (string, error)
}

// CategoryFunc returns the current set of configured category names.
// Called on each scan cycle to pick up dynamic config changes. May be nil.
type CategoryFunc func() []string

// Scanner watches a directory for stable NZB files and decompressed archives,
// extracts them, and passes them to a handler. If a CategoryFunc is provided,
// subdirectories matching category names (case-insensitive) are also scanned
// and files found there inherit that category.
type Scanner struct {
	dir     string
	store   *Store
	handler Handler
	catFn   CategoryFunc
	logger  *slog.Logger
}

// New creates a new Scanner for the given directory.
func New(dir string, store *Store, h Handler, cats CategoryFunc, logger *slog.Logger) *Scanner {
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "dirscanner")
	return &Scanner{
		dir:     dir,
		store:   store,
		handler: h,
		catFn:   cats,
		logger:  log,
	}
}

// buildCategoryMap calls the CategoryFunc and builds a case-insensitive
// lookup map (lowercased name → original name). Returns nil if no
// CategoryFunc is set or it returns no categories.
func (s *Scanner) buildCategoryMap() map[string]string {
	if s.catFn == nil {
		return nil
	}
	names := s.catFn()
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]string, len(names))
	for _, name := range names {
		m[strings.ToLower(name)] = name
	}
	return m
}

// ScanOnce performs one scan of the directory and any category subdirectories.
// It detects stable files (matching size and mtime from a prior scan),
// decompresses them, invokes the handler, and on success deletes the source
// file. Returns the number of files processed.
func (s *Scanner) ScanOnce(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	catMap := s.buildCategoryMap()
	scannedDirs := make(map[string]bool)

	// Scan root directory (no category).
	currentScan, processed, err := s.scanDir(ctx, s.dir, "")
	if err != nil {
		return 0, err
	}
	scannedDirs[filepath.Clean(s.dir)] = true

	// Scan category subdirectories.
	subProcessed, err := s.scanCategorySubdirs(ctx, catMap, scannedDirs, currentScan)
	if err != nil {
		return processed, err
	}
	processed += subProcessed

	// Remove entries from store that no longer exist on disk.
	s.pruneRemovedFiles(scannedDirs, currentScan)

	// Persist updated state to disk.
	if err := s.store.Save(); err != nil {
		s.logger.Warn("failed to save state", "err", err)
	}

	return processed, nil
}

// scanDir scans a single directory for stable NZB files. The category
// parameter is passed through to the handler in FetchOptions; it is empty
// for the root watch directory.
//
// Returns the set of files observed (for store cleanup) and the number
// of files that were successfully processed.
func (s *Scanner) scanDir(ctx context.Context, dir, category string) (files map[string]FileState, nzbCount int, err error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read directory: %w", err)
	}

	processed := 0
	currentScan := make(map[string]FileState)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		// Skip dotfiles and non-matching extensions.
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		if !isValidExtension(entry.Name()) {
			continue
		}

		ok, err := s.processScannedFile(ctx, path, entry.Name(), category, currentScan)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, processed, err
			}
			s.logger.Warn("failed to handle file", "path", path, "err", err)
			continue
		}
		if ok {
			processed++
		}
	}

	return currentScan, processed, nil
}

// handleStableFile extracts NZBs from a stable file and invokes the handler.
func (s *Scanner) handleStableFile(ctx context.Context, path, filename, category string) error {
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

		opts := types.FetchOptions{
			Category: category,
			PP:       types.PPInherit,
			Priority: constants.DefaultPriority,
		}
		if _, err := s.handler.HandleNZB(ctx, label, nzbData, opts); err != nil {
			s.logger.Warn("handler failed for NZB", "label", label, "err", err)
			lastErr = err
			continue
		}

		successCount++
	}

	// Only delete the source file if ALL NZBs succeeded. Partial success
	// leaves the archive for the next scan cycle so the failed NZBs can
	// be retried (e.g. after a transient DB lock or config reload).
	if lastErr != nil {
		if successCount > 0 {
			// Some NZBs imported — leave the archive for retry.
			return &PartialError{Failed: len(nzbs) - successCount, Total: len(nzbs), Err: lastErr}
		}
		return fmt.Errorf("%d of %d NZBs failed: %w", len(nzbs), len(nzbs), lastErr)
	}
	if successCount == 0 {
		return fmt.Errorf("no NZBs processed")
	}

	if err := os.Remove(path); err != nil {
		s.logger.Warn("failed to delete file after successful handling", "path", path, "err", err)
	}
	return nil
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
				if errors.Is(err, context.Canceled) {
					return err
				}
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
		strings.HasSuffix(lower, ".zip")
}

// scanCategorySubdirs scans category-specific watch folders underneath watch root.
func (s *Scanner) scanCategorySubdirs(
	ctx context.Context,
	catMap map[string]string,
	scannedDirs map[string]bool,
	currentScan map[string]FileState,
) (int, error) {
	if catMap == nil {
		return 0, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("failed to read directory for subdirs: %w", err)
	}

	processed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		catName, ok := catMap[strings.ToLower(entry.Name())]
		if !ok {
			continue
		}

		subDir := filepath.Join(s.dir, entry.Name())
		subScan, subProcessed, err := s.scanDir(ctx, subDir, catName)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return processed, err
			}
			s.logger.Warn("failed to scan category subdir", "dir", subDir, "category", catName, "err", err)
			continue
		}
		processed += subProcessed
		scannedDirs[filepath.Clean(subDir)] = true

		// Merge subdir scan results into the combined map.
		maps.Copy(currentScan, subScan)
	}
	return processed, nil
}

// pruneRemovedFiles scans the store for deleted files and watch folders to prune from state.
func (s *Scanner) pruneRemovedFiles(scannedDirs map[string]bool, currentScan map[string]FileState) {
	var toDelete []string
	s.store.mu.RLock()
	for storedPath := range s.store.states {
		if _, exists := currentScan[storedPath]; !exists {
			dir := filepath.Dir(storedPath)
			if scannedDirs[dir] {
				// Directory was scanned but file is gone — prune.
				toDelete = append(toDelete, storedPath)
			} else if _, err := os.Stat(dir); os.IsNotExist(err) {
				// Directory no longer exists (category removed or
				// folder deleted) — prune to prevent unbounded growth.
				toDelete = append(toDelete, storedPath)
			}
		}
	}
	s.store.mu.RUnlock()

	for _, path := range toDelete {
		s.store.Delete(path)
	}
}

// processScannedFile checks if the file at path is stable and, if so,
// processes it (extracts and handles it). Returns true if successfully
// processed, or false if not yet stable, skipped, or failed.
func (s *Scanner) processScannedFile(
	ctx context.Context,
	path string,
	name string,
	category string,
	currentScan map[string]FileState,
) (bool, error) {
	// Get current file stat.
	stat, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("stat: %w", err)
	}

	state := FileState{
		Size:  stat.Size(),
		MTime: stat.ModTime(),
	}
	currentScan[path] = state

	// Check if this file was seen in a prior scan with the same state.
	priorState, wasSeen := s.store.Get(path)
	if !wasSeen {
		// First sighting: record it and return.
		s.store.Set(path, state)
		s.logger.Debug("first sighting of file, waiting to see if it's size changes on the next scan before adding it to queue", "path", path)
		return false, nil
	}

	// File was seen before. Check if it's stable (same size+mtime).
	if priorState.Size != state.Size || !priorState.MTime.Equal(state.MTime) {
		// File changed: reset the stable timer by updating its recorded state.
		s.store.Set(path, state)
		s.logger.Debug("file changed, resetting stability timer", "path", path)
		return false, nil
	}

	// File is stable. Extract, handle, and clean up.
	if err := s.handleStableFile(ctx, path, name, category); err != nil {
		if errors.Is(err, context.Canceled) {
			return false, err
		}
		// Only leave for retry if some NZBs succeeded (partial success).
		// If all failed or extraction itself failed, permanently mark as .failed.
		var pe *PartialError
		if !errors.As(err, &pe) {
			failedPath := path + ".failed"
			if renameErr := os.Rename(path, failedPath); renameErr != nil {
				s.logger.Warn("failed to rename file", "path", path, "err", renameErr)
			}
			s.store.Delete(path)
		}
		return false, err
	}

	return true, nil
}
