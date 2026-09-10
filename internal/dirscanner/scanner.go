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
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/types"
)

// failedDirName is the reserved subdirectory a scanned directory's
// permanently-failed files are moved into. Reserved so scanCategorySubdirs
// never treats it as a category, even if a category happens to share the
// name — see the guard in scanCategorySubdirs.
const failedDirName = "failed"

// PartialError indicates that some but not all NZBs from an archive were
// successfully imported. The scanner uses this to decide whether to leave
// the source file for retry (partial success) or move it into failedDirName
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

	// warnedExtensions remembers the FileState last logged for a path
	// skipped because of an unrecognized extension, so a file the scanner
	// will never touch (it isn't a candidate NZB) is reported once per
	// distinct size+mtime rather than every scan cycle forever. Not
	// persisted: losing it across a restart just costs one extra log line
	// per such file, not correctness. pruneRemovedFiles evicts entries for
	// paths that vanish, alongside the same cleanup it does for s.store.
	warnedExtensions map[string]FileState
}

// New creates a new Scanner for the given directory.
func New(dir string, store *Store, h Handler, cats CategoryFunc, logger *slog.Logger) *Scanner {
	// Leaf packages self-scope only when they are the root of the logger chain;
	// a caller-supplied logger is assumed already scoped.
	if logger == nil {
		logger = slog.Default().With("component", "dirscanner")
	}
	return &Scanner{
		dir:              dir,
		store:            store,
		handler:          h,
		catFn:            cats,
		logger:           logger,
		warnedExtensions: make(map[string]FileState),
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
			if state, ok := s.warnUnrecognizedOnce(path); ok {
				currentScan[path] = state
			}
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
		// Reserved regardless of category config: if a category happened
		// to be named "failed" too, scanning it as a category would
		// re-ingest every file moveToFailedDir ever parked here.
		if strings.EqualFold(entry.Name(), failedDirName) {
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

// warnUnrecognizedOnce logs, at Debug, a file skipped for having no
// recognized NZB extension — but only the first time it's seen with a given
// size+mtime, not on every scan cycle. This file will never be handled (it
// isn't a candidate NZB at all, e.g. an unrelated file left in the watch
// folder), so re-logging it every cycle forever would spam the log about
// something already known, for as long as the file sits there.
//
// Returns the file's current state and whether it could be stat'd, so the
// caller can still record it in currentScan (for pruneRemovedFiles) even on
// a cycle where nothing new was logged. Returns (FileState{}, false) if the
// file vanished between ReadDir and Stat — nothing to warn about or track.
func (s *Scanner) warnUnrecognizedOnce(path string) (FileState, bool) {
	stat, err := os.Stat(path)
	if err != nil {
		return FileState{}, false
	}
	state := FileState{Size: stat.Size(), MTime: stat.ModTime()}
	if prior, seen := s.warnedExtensions[path]; seen && prior.Size == state.Size && prior.MTime.Equal(state.MTime) {
		return state, true
	}
	s.warnedExtensions[path] = state
	s.logger.Debug("skipping file with unrecognized extension", "path", path)
	return state, true
}

// pruneRemovedFiles scans the store for deleted files and watch folders to prune from state.
func (s *Scanner) pruneRemovedFiles(scannedDirs map[string]bool, currentScan map[string]FileState) {
	var toDelete []string
	s.store.mu.RLock()
	for storedPath := range s.store.states {
		if _, exists := currentScan[storedPath]; !exists && isGoneFromScannedDir(storedPath, scannedDirs) {
			toDelete = append(toDelete, storedPath)
		}
	}
	s.store.mu.RUnlock()

	for _, path := range toDelete {
		s.store.Delete(path)
	}

	// Same cleanup for warnedExtensions, which tracks unrecognized-extension
	// files outside s.store entirely — otherwise a file skipped once and
	// then deleted from the watch folder leaks its entry here forever.
	for warnedPath := range s.warnedExtensions {
		if _, exists := currentScan[warnedPath]; !exists && isGoneFromScannedDir(warnedPath, scannedDirs) {
			delete(s.warnedExtensions, warnedPath)
		}
	}
}

// isGoneFromScannedDir reports whether storedPath should be treated as
// removed: either its directory was scanned this cycle and the path simply
// wasn't found in it, or its directory no longer exists at all (a category
// removed, or the folder deleted out from under the scanner).
func isGoneFromScannedDir(storedPath string, scannedDirs map[string]bool) bool {
	dir := filepath.Dir(storedPath)
	if scannedDirs[dir] {
		return true
	}
	_, err := os.Stat(dir)
	return os.IsNotExist(err)
}

// moveToFailedDir moves path into a failedDirName subdirectory alongside it,
// so a permanently-failed file stops being a scan candidate at all instead
// of sitting in the scanned directory under a name (the old behavior:
// path+".failed") that scanDir would still enumerate — and, since it warns
// about unrecognized extensions, would have kept re-logging every cycle.
// scanDir never descends into subdirectories it isn't told to treat as a
// category (see the reserved-name guard in scanCategorySubdirs for the one
// case that needs an explicit exception), so failedDirName is already
// excluded from every future scan without further bookkeeping. Logs and
// gives up on any error — this path already failed once and is not worth
// blocking the rest of the scan over.
func (s *Scanner) moveToFailedDir(path string) {
	failedDir := filepath.Join(filepath.Dir(path), failedDirName)
	if err := os.MkdirAll(failedDir, 0o750); err != nil {
		s.logger.Warn("failed to create failed/ directory", "dir", failedDir, "err", err)
		return
	}
	target := fsutil.GetUniqueFilename(filepath.Join(failedDir, filepath.Base(path)))
	if err := os.Rename(path, target); err != nil {
		s.logger.Warn("failed to move file to failed/ directory", "path", path, "target", target, "err", err)
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
		// If all failed or extraction itself failed, move it into
		// failedDirName so it stops being a scan candidate entirely,
		// rather than an in-place rename that scanDir would keep seeing
		// (and, before this, re-logging) every cycle forever.
		if _, ok := errors.AsType[*PartialError](err); !ok {
			s.moveToFailedDir(path)
			s.store.Delete(path)
		}
		return false, err
	}

	return true, nil
}
