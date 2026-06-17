package postproc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ExtensionCleanupStage deletes files whose extension matches a user-
// configured list from job.DownloadDir after unpack has run. Mirrors
// SABnzbd's postproc.py:cleanup_list() — extension-based cleanup runs
// after deobfuscation and sorting, before the final move.
//
// Extensions are case-insensitive and should be stored without a leading
// dot (e.g. "nfo", "txt", "sfv"). An NZB extension in the cleanup list
// is skipped because NZBs may be re-queued.
type ExtensionCleanupStage struct {
	mu sync.RWMutex
	// Extensions is the list of extensions (without dot) to delete.
	Extensions []string
	// SkipNZB prevents removal of .nzb files even if listed.
	SkipNZB bool
	Log     *slog.Logger
}

// NewExtensionCleanupStage constructs an ExtensionCleanupStage.
func NewExtensionCleanupStage(extensions []string) *ExtensionCleanupStage {
	s := &ExtensionCleanupStage{
		SkipNZB: true,
	}
	s.SetExtensions(extensions)
	return s
}

// SetExtensions thread-safely updates the extensions to be cleaned up.
func (s *ExtensionCleanupStage) SetExtensions(extensions []string) {
	// Normalize: lowercase and strip leading dots.
	normed := make([]string, 0, len(extensions))
	for _, ext := range extensions {
		ext = strings.TrimSpace(ext)
		ext = strings.TrimLeft(ext, ".")
		ext = strings.ToLower(ext)
		if ext != "" {
			normed = append(normed, ext)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Extensions = normed
}

// Name implements Stage.
func (*ExtensionCleanupStage) Name() string { return "extension_cleanup" }

// Run walks job.DownloadDir recursively, matches file extensions against
// the configured cleanup list, and deletes matches. Best-effort: errors
// from individual deletions are logged but don't fail the pipeline.
// After deletion, removes any newly-empty subdirectories.
func (s *ExtensionCleanupStage) Run(ctx context.Context, job *Job) error {
	log := s.logger(job)

	s.mu.RLock()
	extensions := s.Extensions
	s.mu.RUnlock()

	if len(extensions) == 0 {
		return nil
	}

	// Build a set for O(1) lookup.
	extSet := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		extSet[ext] = struct{}{}
	}

	var removed int
	walkErr := filepath.WalkDir(job.DownloadDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		ext := strings.TrimPrefix(filepath.Ext(d.Name()), ".")
		ext = strings.ToLower(ext)

		// If the file has no extension, check if the whole filename is in
		// the cleanup list (SABnzbd behavior: cleanup_ext == name).
		if ext == "" {
			ext = strings.ToLower(strings.TrimSpace(d.Name()))
		}

		if _, ok := extSet[ext]; !ok {
			return nil
		}
		if s.SkipNZB && ext == "nzb" {
			return nil
		}
		// M7: protect files consumed by repair/join operations.
		if _, consumed := job.ConsumedFiles[path]; consumed {
			return nil
		}

		if err := os.Remove(path); err != nil { //nolint:gosec // operating within the job-owned output dir
			log.Warn("failed to remove file", "path", path, "err", err)
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("remove %s: %v", path, err))
			return nil
		}
		removed++
		log.Info("removed unwanted file", "path", path, "ext", ext)
		job.OutputLines = append(job.OutputLines,
			fmt.Sprintf("removed %s (ext=%s)", filepath.Base(path), ext))
		return nil
	})
	if walkErr != nil {
		log.Warn("extension cleanup walk failed", "dir", job.DownloadDir, "err", walkErr)
		job.OutputLines = append(job.OutputLines,
			fmt.Sprintf("walk %s: %v", job.DownloadDir, walkErr))
		return nil
	}

	// Clean up any now-empty directories (like SABnzbd's cleanup_empty_directories).
	cleanupEmptyDirs(job.DownloadDir)

	if removed > 0 {
		log.Info("extension cleanup complete", "removed", removed)
	}
	return nil
}

// cleanupEmptyDirs removes empty subdirectories under root, bottom-up.
// It does not remove root itself.
func cleanupEmptyDirs(root string) {
	// Walk bottom-up by collecting dirs first, then processing in reverse.
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			//nolint:nilerr // ignore walk errors to do a best-effort empty directory cleanup
			return nil
		}
		if !d.IsDir() || path == root {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})
	// Process deepest first.
	for i := len(dirs) - 1; i >= 0; i-- {
		entries, err := os.ReadDir(dirs[i])
		if err == nil && len(entries) == 0 {
			_ = os.Remove(dirs[i])
		}
	}
}

func (s *ExtensionCleanupStage) logger(job *Job) *slog.Logger {
	if s.Log != nil {
		return s.Log.With("job", job.Queue.ID, "stage", s.Name())
	}
	return slog.Default().With("job", job.Queue.ID, "stage", s.Name())
}
