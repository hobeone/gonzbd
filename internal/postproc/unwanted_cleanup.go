package postproc

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/unwanted"
)

// UnwantedCleanupStage deletes the job's files whose extension the
// unwanted-extension rules exclude, once unpack, par2 renaming and
// deobfuscation have given them their final names. Mirrors SABnzbd's
// postproc.remove_unwanted_files.
//
// It is the backstop for what the ingest check cannot see: a file inside an
// archive, a file whose NZB subject was obfuscated, a file par2 or
// deobfuscation renamed. It reads every file the job owns (every file under
// DownloadDir when OwnedFiles is nil), not only what unpack extracted.
//
// It removes nothing when:
//   - the rules' action is off;
//   - the job is approved (unwanted.StateApproved): the user accepted the
//     files, and SABnzbd's unwanted_ext == 2 skips its removal the same way;
//   - the job has already failed (ParError, UnpackError or FailMsg): its
//     files stay in the download area for a retry, and it delivers nothing
//     to the complete directory.
//
// It runs at every PP level, unlike SABnzbd, which removes only after its
// unpack step: a download-only job delivers its files to the complete
// directory as well.
//
// If the rules cannot be read it fails the job rather than deliver files it
// did not check.
type UnwantedCleanupStage struct {
	// rules returns the live rules; read once per run, so a settings change
	// applies from the next job.
	rules func() (unwanted.Rules, error)
	Log   *slog.Logger
}

// NewUnwantedCleanupStage constructs the stage over a rules source.
func NewUnwantedCleanupStage(rules func() (unwanted.Rules, error)) *UnwantedCleanupStage {
	return &UnwantedCleanupStage{rules: rules}
}

// Name implements Stage.
func (*UnwantedCleanupStage) Name() string { return "unwanted_cleanup" }

// Run implements Stage.
func (s *UnwantedCleanupStage) Run(ctx context.Context, job *Job) error {
	log := s.logger(job)
	if job.Unwanted == unwanted.StateApproved {
		logf(ctx, log, job, slog.LevelInfo, "Skipped: the job was approved with its unwanted extensions")
		return nil
	}
	if job.ParError || job.UnpackError || job.FailMsg != "" {
		return nil
	}
	rules, err := s.rules()
	if err != nil {
		job.FailMsg = fmt.Sprintf("unwanted-extension check could not run: %v", err)
		return fmt.Errorf("unwanted_cleanup: %w", err)
	}
	if rules.Action() == unwanted.ActionOff {
		return nil
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
		if d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !rules.Unwanted(d.Name()) {
			return nil
		}
		// Only the job's own files, as the other cleanup stages; a nil
		// OwnedFiles means untracked (see Job.OwnedFiles).
		absPath := filepath.Join(job.DownloadDir, path)
		if job.OwnedFiles != nil {
			if _, owned := job.OwnedFiles[absPath]; !owned {
				return nil
			}
		}
		if err := root.Remove(path); err != nil {
			logf(ctx, log, job, slog.LevelWarn, "remove %s: %v", path, err)
			return nil
		}
		delete(job.OwnedFiles, absPath)
		removed++
		logf(ctx, log, job, slog.LevelInfo, "removed %s (unwanted extension)", path)
		return nil
	})
	if walkErr != nil {
		logf(ctx, log, job, slog.LevelWarn, "walk %s: %v", job.DownloadDir, walkErr)
	}
	if removed > 0 {
		cleanupEmptyDirs(root)
		logf(ctx, log, job, slog.LevelInfo, "Removed %d files with unwanted extensions", removed)
	}
	return nil
}

func (s *UnwantedCleanupStage) logger(job *Job) *slog.Logger {
	l := s.Log
	if l == nil {
		l = slog.Default()
	}
	if id := job.JobID(); id != "" {
		return l.With("job", id, "stage", s.Name())
	}
	return l.With("stage", s.Name())
}
