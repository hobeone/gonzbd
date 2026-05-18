package postproc

import (
	"context"
	"log/slog"
	"os"

	"github.com/hobeone/gonzbd/internal/par2"
)

// Par2CleanupStage deletes .par2 files and par2-created backup files from
// the job's download directory. It runs after unpack and only proceeds when
// both repair and unpack succeeded (no ParError, no UnpackError). This
// preserves par2 files for debugging when extraction fails — previously
// they were deleted inside RepairStage before unpack even ran.
type Par2CleanupStage struct {
	// Cleanup controls whether par2 files should be deleted at all.
	// Maps to the enable_par_cleanup config option.
	Cleanup bool
	Log     *slog.Logger
}

// NewPar2CleanupStage constructs a Par2CleanupStage.
func NewPar2CleanupStage(cleanup bool) *Par2CleanupStage {
	return &Par2CleanupStage{Cleanup: cleanup}
}

// Name implements Stage.
func (*Par2CleanupStage) Name() string { return "par2_cleanup" }

// Run deletes all par2 files and par2 backup files from job.DownloadDir.
// Skipped when Cleanup is false, or when repair or unpack has failed.
func (s *Par2CleanupStage) Run(_ context.Context, job *Job) error {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "postproc/par2_cleanup", "job", job.Queue.ID)

	if !s.Cleanup {
		return nil
	}

	if job.ParError {
		logf(log, job, slog.LevelInfo, "Keeping par2 files (repair failed)")
		return nil
	}
	if job.UnpackError {
		logf(log, job, slog.LevelInfo, "Keeping par2 files (unpack failed)")
		return nil
	}

	// Delete all .par2 files.
	sets, err := par2.FindPar2Files(job.DownloadDir)
	if err != nil {
		log.Warn("par2 cleanup: failed to scan for par2 files", "err", err)
		return nil
	}

	var cleaned int
	for _, set := range sets {
		if set.MainFile != "" {
			_ = os.Remove(set.MainFile)
			cleaned++
		}
		for _, ef := range set.ExtraFiles {
			_ = os.Remove(ef)
			cleaned++
		}
	}
	if cleaned > 0 {
		logf(log, job, slog.LevelInfo, "Cleaned up %d par2 file(s)", cleaned)
	}

	// Par2 repair creates backup copies of damaged files by appending
	// ".1", ".2" etc. (e.g. "movie.part01.rar" → "movie.part01.rar.1").
	// These orphaned backups confuse later stages (deobfuscate sees
	// RAR magic bytes in a ".1" file and incorrectly appends ".rar").
	backups := cleanupPar2Backups(job.DownloadDir, log)
	if backups > 0 {
		logf(log, job, slog.LevelInfo, "Cleaned up %d par2 backup file(s)", backups)
	}

	return nil
}
