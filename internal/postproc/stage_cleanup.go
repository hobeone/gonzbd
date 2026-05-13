package postproc

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
)

type CleanupStage struct {
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewCleanupStage constructs a CleanupStage.
func NewCleanupStage() *CleanupStage { return &CleanupStage{} }

// Name returns the stage identifier.
func (*CleanupStage) Name() string { return "cleanup" }

// Run removes admin sidecar data from the download directory.
func (c *CleanupStage) Run(_ context.Context, job *Job) error {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "postproc/cleanup", "job", job.Queue.ID)

	// On failure, preserve admin data for debugging/retry.
	if job.ParError || job.UnpackError || job.FailMsg != "" {
		logf(log, job, slog.LevelInfo, "Skipped: preserving admin data for failed job")
		return nil
	}

	adminDir := filepath.Join(job.DownloadDir, "__ADMIN__")
	if err := os.RemoveAll(adminDir); err != nil {
		log.Warn("cleanup: failed to remove admin dir",
			"dir", adminDir, "err", err)
		// Non-fatal — the job already succeeded.
		return nil
	}

	if _, err := os.Stat(adminDir); err == nil {
		// Directory still exists after RemoveAll — shouldn't happen,
		// but log it rather than failing.
		log.Warn("cleanup: admin dir still exists after removal",
			"dir", adminDir)
	}

	logf(log, job, slog.LevelInfo, "Removed admin data")
	return nil
}
