package postproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// FinalizeStage moves the completed job to its final destination directory.
type FinalizeStage struct {
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
	// folderRename enables the _UNPACK_/_FAILED_ prefix behavior.
	// Atomic so SetFolderRename can be called from any goroutine.
	folderRename atomic.Bool
}

// NewFinalizeStage constructs a FinalizeStage.
func NewFinalizeStage() *FinalizeStage { return &FinalizeStage{} }

// SetFolderRename enables or disables the _UNPACK_/_FAILED_ prefix behavior
// at runtime without restart. Thread-safe.
func (f *FinalizeStage) SetFolderRename(enabled bool) { f.folderRename.Store(enabled) }

// Name returns the stage identifier.
func (*FinalizeStage) Name() string { return "finalize" }

// Run moves the directory content or the directory itself to its final location.
func (f *FinalizeStage) Run(ctx context.Context, job *Job) error {
	log := f.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "finalize", "job", job.Queue.ID)

	// Snapshot once; used in multiple branches below.
	folderRename := f.folderRename.Load()

	if job.ParError || job.UnpackError || job.FailMsg != "" {
		return f.handleFailure(ctx, log, job, folderRename)
	}

	if job.FinalDir == "" {
		return fmt.Errorf("finalize: FinalDir not set")
	}

	if job.DownloadDir == job.FinalDir {
		logf(ctx, log, job, slog.LevelInfo, "Already at final location: %s", job.FinalDir)
		return nil // Already there (e.g. one-shot download directly to target)
	}

	// Determine the initial destination — with _UNPACK_ prefix if enabled.
	dest := job.FinalDir
	if folderRename {
		dest = prefixDirName(job.FinalDir, "_UNPACK_")
	}

	logf(ctx, log, job, slog.LevelInfo, "Moving %s → %s", job.DownloadDir, dest)
	return f.moveToDest(ctx, log, job, dest, folderRename)
}

func (f *FinalizeStage) handleFailure(ctx context.Context, log *slog.Logger, job *Job, folderRename bool) error {
	var reasons []string
	if job.ParError {
		reasons = append(reasons, "repair failed")
	}
	if job.UnpackError {
		reasons = append(reasons, "unpack failed")
	}
	if job.FailMsg != "" {
		reasons = append(reasons, job.FailMsg)
	}

	// When FolderRename is enabled, rename the download directory
	// in-place with a _FAILED_ prefix so users can visually identify
	// it. The files stay in the incomplete/download area (NOT moved
	// to complete) so that retry can find them.
	if folderRename && job.DownloadDir != "" {
		failedDir := prefixDirName(job.DownloadDir, "_FAILED_")
		if err := os.Rename(job.DownloadDir, failedDir); err == nil {
			logf(ctx, log, job, slog.LevelInfo, "Renamed to %s", failedDir)
			job.DownloadDir = failedDir
		} else {
			logf(ctx, log, job, slog.LevelWarn, "Failed to add _FAILED_ prefix: %v", err)
		}
	}

	logf(ctx, log, job, slog.LevelInfo, "Skipped final move: files remain in download directory (%s)", strings.Join(reasons, "; "))
	return nil // Skip move if failed, so files stay in DownloadDir for retry
}

func (f *FinalizeStage) moveToDest(ctx context.Context, log *slog.Logger, job *Job, dest string, folderRename bool) error {
	// Create parent directory for final destination
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("finalize: mkdir %s: %w", filepath.Dir(dest), err)
	}

	// If the source directory exists, rename it to the target.
	// Fall back to file-by-file move on cross-device (EXDEV) or
	// not-empty (ENOTEMPTY/EEXIST) errors — the latter allows
	// merging files into an existing destination directory.
	if err := os.Rename(job.DownloadDir, dest); err == nil {
		logf(ctx, log, job, slog.LevelInfo, "%s → %s (atomic rename)", job.DownloadDir, dest)
		job.DownloadDir = dest
		// If FolderRename is active, strip the _UNPACK_ prefix now.
		if folderRename {
			if err := os.Rename(dest, job.FinalDir); err != nil {
				logf(ctx, log, job, slog.LevelWarn, "Failed to strip _UNPACK_ prefix: %v", err)
				// Not fatal — files are in _UNPACK_ dir but accessible.
			} else {
				logf(ctx, log, job, slog.LevelInfo, "%s → %s (prefix stripped)", dest, job.FinalDir)
				job.DownloadDir = job.FinalDir
			}
		}
		return nil
	} else if !fsutil.IsRenameMergeNeeded(err) {
		return fmt.Errorf("finalize: rename %s -> %s: %w", job.DownloadDir, dest, err)
	} else {
		logf(ctx, log, job, slog.LevelInfo, "Atomic rename failed (%v), falling back to file-by-file move", err)
	}

	return f.moveFileByFile(ctx, log, job, dest, folderRename)
}

func (f *FinalizeStage) moveFileByFile(ctx context.Context, log *slog.Logger, job *Job, dest string, folderRename bool) error {
	// Fallback: If os.Rename failed (e.g. cross-device), move file by file.
	entries, err := os.ReadDir(job.DownloadDir)
	if err != nil {
		return fmt.Errorf("finalize: readdir %s: %w", job.DownloadDir, err)
	}

	if err := os.MkdirAll(dest, 0o750); err != nil {
		return fmt.Errorf("finalize: mkdir %s: %w", dest, err)
	}

	var moveErrors []error
	for _, e := range entries {
		src := filepath.Join(job.DownloadDir, e.Name())
		dst := fsutil.JoinSafe(dest, "", e.Name(), job.Sanitize)
		if err := moveRecursive(ctx, src, dst); err != nil {
			moveErrors = append(moveErrors, fmt.Errorf("finalize: move %s -> %s: %w", src, dst, err))
			logf(ctx, log, job, slog.LevelWarn, "Failed to move %s → %s: %v", filepath.Base(src), dst, err)
			continue
		}
		logf(ctx, log, job, slog.LevelInfo, "%s → %s", filepath.Base(src), dst)
	}

	if len(moveErrors) > 0 {
		// Some files failed to move — do NOT remove the source directory
		// to avoid data loss of the unmoved files.
		logf(ctx, log, job, slog.LevelWarn, "Partial move: %d file(s) failed, keeping source directory %s", len(moveErrors), job.DownloadDir)
		return errors.Join(moveErrors...)
	}

	// All files moved successfully — clean up the empty source directory.
	_ = fsutil.RemoveAll(job.DownloadDir)
	logf(ctx, log, job, slog.LevelInfo, "Removed empty source directory: %s", job.DownloadDir)

	// Update DownloadDir.
	job.DownloadDir = dest

	// Strip _UNPACK_ prefix if FolderRename is active.
	if folderRename {
		if err := os.Rename(dest, job.FinalDir); err != nil {
			logf(ctx, log, job, slog.LevelWarn, "Failed to strip _UNPACK_ prefix: %v", err)
		} else {
			logf(ctx, log, job, slog.LevelInfo, "%s → %s (prefix stripped)", dest, job.FinalDir)
			job.DownloadDir = job.FinalDir
		}
	}

	return nil
}

// prefixDirName prepends a prefix to the last path component of dir.
// Example: prefixDirName("/complete/movies/MyRelease", "_UNPACK_")
// returns "/complete/movies/_UNPACK_MyRelease".
func prefixDirName(dir, prefix string) string {
	parent, base := filepath.Split(dir)
	return filepath.Join(parent, prefix+base)
}

// moveRecursive handles moving files or directories, with cross-device support.
func moveRecursive(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	info, err := os.Lstat(src)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return fsutil.MoveFile(src, dst)
	}

	// It's a directory — preserve source permissions.
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if err := moveRecursive(ctx, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}

	return fsutil.Remove(src)
}

// listNonPar2Files returns the full paths of all regular (non-directory)
// files in dir that are not .par2 files. These are passed as extra
// arguments to par2 repair so it can checksum-match files regardless of
// whether their names match the par2 set's file list.
