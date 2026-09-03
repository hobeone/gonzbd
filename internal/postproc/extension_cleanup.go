package postproc

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

	root, err := os.OpenRoot(job.DownloadDir)
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "open root %s: %v", job.DownloadDir, err)
		return nil
	}
	defer root.Close() //nolint:errcheck // read-only close

	var removed int
	walkErr := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path == "." {
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
		absPath := filepath.Join(job.DownloadDir, path)
		if _, consumed := job.ConsumedFiles[absPath]; consumed {
			return nil
		}
		// dupcomment:ok the extension and sample cleanups apply one shared
		// ownership restriction; the rationale must stay identical in both.
		//
		// Restrict deletion to files this job actually owns (upstream
		// SABnzbd #3462). A nil OwnedFiles means "not tracked" and disables
		// the restriction (see Job.OwnedFiles doc comment).
		if job.OwnedFiles != nil {
			if _, owned := job.OwnedFiles[absPath]; !owned {
				return nil
			}
		}

		if err := root.Remove(path); err != nil {
			logf(ctx, log, job, slog.LevelWarn, "remove %s: %v", absPath, err)
			return nil
		}
		removed++
		logf(ctx, log, job, slog.LevelInfo, "removed %s (ext=%s)", filepath.Base(absPath), ext)
		return nil
	})
	if walkErr != nil {
		logf(ctx, log, job, slog.LevelWarn, "walk %s: %v", job.DownloadDir, walkErr)
		return nil
	}

	// Clean up any now-empty directories (like SABnzbd's cleanup_empty_directories).
	cleanupEmptyDirs(root)

	if removed > 0 {
		logf(ctx, log, job, slog.LevelInfo, "extension cleanup complete: removed %d file(s)", removed)
	}
	return nil
}

// cleanupEmptyDirs removes empty subdirectories under root, bottom-up.
// It does not remove root itself.
func cleanupEmptyDirs(root *os.Root) {
	// Walk bottom-up by collecting dirs first, then processing in reverse.
	var dirs []string
	_ = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			//nolint:nilerr // ignore walk errors to do a best-effort empty directory cleanup
			return nil
		}
		if !d.IsDir() || path == "." {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})
	// Process deepest first.
	for _, dirPath := range slices.Backward(dirs) {
		d, err := root.Open(dirPath)
		if err == nil {
			entries, rerr := d.ReadDir(-1)
			_ = d.Close()
			if rerr == nil && len(entries) == 0 {
				_ = root.Remove(dirPath)
			}
		}
	}
}

func (s *ExtensionCleanupStage) logger(job *Job) *slog.Logger {
	if s.Log != nil {
		if job.Job != nil {
			return s.Log.With("job", job.Job.ID(), "stage", s.Name())
		}
		return s.Log.With("stage", s.Name())
	}
	if job.Job != nil {
		return slog.Default().With("job", job.Job.ID(), "stage", s.Name())
	}
	return slog.Default().With("stage", s.Name())
}
