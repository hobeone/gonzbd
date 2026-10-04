package postproc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/unwanted"
)

// UnwantedCleanupStage deletes the files under the job's DownloadDir whose
// extension the unwanted-extension rules exclude, once unpack, par2 repair
// and renaming, and deobfuscation have given them their final names. Mirrors
// SABnzbd's postproc.remove_unwanted_files.
//
// It is the backstop for what the ingest check cannot see: a file inside an
// archive, a file whose NZB subject was obfuscated, a file par2 rebuilt or
// renamed. It judges what finalize will deliver, which is everything under
// DownloadDir, so it does not consult OwnedFiles: par2 repair records
// nothing there. DownloadDir is the download directory joined with the job's
// name, and no two registered jobs share a name: the dispatcher refuses one
// another registered job holds, both when a job is registered and when it is
// renamed (Dispatcher.nameHolderLocked). AddJob and Application.RenameJob
// choose a name with nothing on disk under it (jobNameTaken), and choose
// again when refused; Dispatcher.SetName also refuses an empty name, ".",
// "..", a separator and NUL. A retry takes the name rebuilt from its history
// entry, not a freshly chosen one, and is refused if another job holds it.
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
// If the rules cannot be read, or a file cannot be read or removed, it fails
// the job rather than deliver files it did not check.
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
		return s.fail(job, fmt.Errorf("open %s: %w", job.DownloadDir, err))
	}
	defer root.Close() //nolint:errcheck // read-only close

	var removed int
	var errs []error
	walkErr := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Record it and keep walking, so one unreadable directory
			// does not hide the files after it; the job fails below.
			errs = append(errs, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !rules.Unwanted(d.Name()) {
			return nil
		}
		if err := root.Remove(path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
			return nil
		}
		delete(job.OwnedFiles, filepath.Join(job.DownloadDir, path))
		removed++
		logf(ctx, log, job, slog.LevelInfo, "removed %s (unwanted extension)", path)
		return nil
	})
	if walkErr != nil {
		// The callback returns an error only on cancellation.
		return walkErr
	}
	if removed > 0 {
		cleanupEmptyDirs(root)
		logf(ctx, log, job, slog.LevelInfo, "Removed %d files with unwanted extensions", removed)
	}
	if err := errors.Join(errs...); err != nil {
		return s.fail(job, err)
	}
	return nil
}

// fail fails the job for a file the stage could not check or remove: finalize
// would deliver it unchecked.
func (s *UnwantedCleanupStage) fail(job *Job, err error) error {
	job.FailMsg = fmt.Sprintf("unwanted-extension cleanup could not check every file: %v", err)
	return fmt.Errorf("unwanted_cleanup: %w", err)
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
