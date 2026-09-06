package postproc

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync/atomic"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/par2"
)

// Par2CleanupStage deletes .par2 files and par2-created backup files from
// the job's download directory. It runs after unpack and only proceeds when
// both repair and unpack succeeded (no ParError, no UnpackError). This
// preserves par2 files for debugging when extraction fails — previously
// they were deleted inside RepairStage before unpack even ran.
type Par2CleanupStage struct {
	// cleanup is set atomically so SetCleanup can be called from any goroutine
	// (e.g. the API handler) while a job may be running in the postproc worker.
	cleanup   atomic.Bool
	ParseOpts par2.ParseOptions
	Log       *slog.Logger
}

// NewPar2CleanupStage constructs a Par2CleanupStage.
func NewPar2CleanupStage(cleanup bool) *Par2CleanupStage {
	s := &Par2CleanupStage{}
	s.cleanup.Store(cleanup)
	return s
}

// SetCleanup enables or disables par2 file deletion at runtime without
// requiring a server restart. Thread-safe; may be called from any goroutine.
func (s *Par2CleanupStage) SetCleanup(enabled bool) { s.cleanup.Store(enabled) }

// CleanupEnabled reports whether par2 file deletion is active. Thread-safe.
func (s *Par2CleanupStage) CleanupEnabled() bool { return s.cleanup.Load() }

// Name implements Stage.
func (*Par2CleanupStage) Name() string { return "par2_cleanup" }

// Run deletes all par2 files and par2 backup files from job.DownloadDir.
// Skipped when Cleanup is false, or when repair or unpack has failed.
func (s *Par2CleanupStage) Run(ctx context.Context, job *Job) error {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "par2_cleanup", "job", job.JobID())

	if !s.cleanup.Load() {
		return nil
	}

	if job.ParError {
		logf(ctx, log, job, slog.LevelInfo, "Keeping par2 files (repair failed)")
		return nil
	}
	if job.UnpackError {
		logf(ctx, log, job, slog.LevelInfo, "Keeping par2 files (unpack failed)")
		return nil
	}

	// Delete all .par2 files.
	sets, err := par2.FindPar2Files(job.DownloadDir, s.ParseOpts)
	if err != nil {
		log.Warn("par2 cleanup: failed to scan for par2 files", "err", err)
		return nil
	}

	var cleaned int
	for _, set := range sets {
		if set.MainFile != "" {
			if err := fsutil.Remove(set.MainFile); err == nil {
				line := "Deleted par2 file: " + filepath.Base(set.MainFile)
				job.OutputLines = append(job.OutputLines, "[par2_cleanup] "+line)
				if job.OnOutput != nil {
					job.OnOutput("par2_cleanup", line)
				}
				cleaned++
			}
		}
		for _, ef := range set.ExtraFiles {
			if err := fsutil.Remove(ef); err == nil {
				line := "Deleted par2 file: " + filepath.Base(ef)
				job.OutputLines = append(job.OutputLines, "[par2_cleanup] "+line)
				if job.OnOutput != nil {
					job.OnOutput("par2_cleanup", line)
				}
				cleaned++
			}
		}
	}
	if cleaned > 0 {
		logf(ctx, log, job, slog.LevelInfo, "Cleaned up %d par2 file(s)", cleaned)
	}

	// Par2 repair creates backup copies of damaged files by appending
	// ".1", ".2" etc. (e.g. "movie.part01.rar" → "movie.part01.rar.1").
	// These orphaned backups confuse later stages (deobfuscate sees
	// RAR magic bytes in a ".1" file and incorrectly appends ".rar").
	backups := cleanupPar2Backups(job.DownloadDir, log)
	for _, backup := range backups {
		line := "Deleted par2 backup file: " + filepath.Base(backup)
		job.OutputLines = append(job.OutputLines, "[par2_cleanup] "+line)
		if job.OnOutput != nil {
			job.OnOutput("par2_cleanup", line)
		}
	}
	if len(backups) > 0 {
		logf(ctx, log, job, slog.LevelInfo, "Cleaned up %d par2 backup file(s)", len(backups))
	}

	return nil
}
