package postproc

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
)

// reSample matches files SABnzbd considers samples or proofs. The
// pattern is anchored on a word boundary (start-of-string OR a
// non-word/underscore character) followed by "sample" or "proof",
// case-insensitive. Mirrors Python sabnzbd/misc.py:RE_SAMPLE.
//
// Examples that match: "movie-sample.mkv", "Sample.mp4",
// "show.S01E01.proof.avi", "release_sample.iso".
// Examples that do NOT match: "examplefile.mp4" (no word boundary
// before "ample"), "approof.txt" (no word boundary before "proof").
var reSample = regexp.MustCompile(`(?i)(^|[\W_])(sample|proof)`)

// IsSample reports whether the given filename looks like a sample or
// proof file. Operates on the basename only — strip directory parts
// before calling if you have a full path.
func IsSample(name string) bool {
	return reSample.MatchString(name)
}

// SampleCleanupStage deletes sample/proof files from job.DownloadDir
// after unpack has run. Mirrors Python sabnzbd/postproc.py:
// remove_samples — when ignore_samples=true, walk the workdir, match
// each filename against IsSample, and delete matches.
//
// False-positive guard: if EVERY file in the workdir matches the
// sample pattern, the stage is a no-op. Catches the case where the
// whole release was distributed as a "sample" archive (e.g. a leak)
// and the user genuinely wants to keep those files.
type SampleCleanupStage struct {
	// toggle provides the thread-safe SetEnabled/enabled flag.
	toggle
	Log *slog.Logger
}

// NewSampleCleanupStage constructs a SampleCleanupStage.
func NewSampleCleanupStage() *SampleCleanupStage {
	return &SampleCleanupStage{}
}

// Name implements Stage.
func (*SampleCleanupStage) Name() string { return "sample_cleanup" }

// Run walks job.DownloadDir recursively, collects sample matches, and
// deletes them. Honors ctx cancellation between deletions but never
// fails the pipeline — best-effort cleanup is preferred over a
// failed-job state since the user data is already extracted.
func (s *SampleCleanupStage) Run(ctx context.Context, job *Job) error {
	if !s.enabled() {
		return nil
	}

	log := s.logger(job)

	root, err := os.OpenRoot(job.DownloadDir)
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "open root %s: %v", job.DownloadDir, err)
		return nil
	}
	defer root.Close() //nolint:errcheck // read-only close

	var samples []string
	totalFiles := 0
	walkErr := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path == "." {
			return nil
		}
		totalFiles++
		if IsSample(d.Name()) {
			samples = append(samples, path)
		}
		return nil
	})
	if walkErr != nil {
		logf(ctx, log, job, slog.LevelWarn, "walk %s: %v", job.DownloadDir, walkErr)
		return nil
	}

	if len(samples) == 0 {
		log.Debug("no sample files found", "dir", job.DownloadDir)
		return nil
	}
	if len(samples) >= totalFiles {
		// Every file looks like a sample — refuse to wipe the whole
		// release. Matches Python's "false-positive" branch.
		logf(ctx, log, job, slog.LevelInfo,
			"sample cleanup skipped: %d/%d files match (false positive guard)",
			len(samples), totalFiles)
		return nil
	}

	for _, p := range samples {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		absPath := filepath.Join(job.DownloadDir, p)
		// dupcomment:ok the extension and sample cleanups apply one shared
		// ownership restriction; the rationale must stay identical in both.
		//
		// Restrict deletion to files this job actually owns (upstream
		// SABnzbd #3462). A nil OwnedFiles means "not tracked" and disables
		// the restriction (see Job.OwnedFiles doc comment).
		if job.OwnedFiles != nil {
			if _, owned := job.OwnedFiles[absPath]; !owned {
				continue
			}
		}
		if err := root.Remove(p); err != nil {
			logf(ctx, log, job, slog.LevelWarn, "remove %s: %v", absPath, err)
			continue
		}
		logf(ctx, log, job, slog.LevelInfo, "removed sample %s", absPath)
	}
	return nil
}

func (s *SampleCleanupStage) logger(job *Job) *slog.Logger {
	if s.Log != nil {
		if job.Queue != nil {
			return s.Log.With("job", job.Queue.ID, "stage", s.Name())
		}
		return s.Log.With("stage", s.Name())
	}
	if job.Queue != nil {
		return slog.Default().With("job", job.Queue.ID, "stage", s.Name())
	}
	return slog.Default().With("stage", s.Name())
}
