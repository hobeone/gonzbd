package postproc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
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
	mu       sync.RWMutex
	disabled bool
	Log      *slog.Logger
}

// NewSampleCleanupStage constructs a SampleCleanupStage.
func NewSampleCleanupStage() *SampleCleanupStage {
	return &SampleCleanupStage{}
}

// SetEnabled enables or disables sample cleanup at runtime. Thread-safe.
func (s *SampleCleanupStage) SetEnabled(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disabled = !v
}

// Name implements Stage.
func (*SampleCleanupStage) Name() string { return "sample_cleanup" }

// Run walks job.DownloadDir recursively, collects sample matches, and
// deletes them. Honors ctx cancellation between deletions but never
// fails the pipeline — best-effort cleanup is preferred over a
// failed-job state since the user data is already extracted.
func (s *SampleCleanupStage) Run(ctx context.Context, job *Job) error {
	s.mu.RLock()
	disabled := s.disabled
	s.mu.RUnlock()

	if disabled {
		return nil
	}

	log := s.logger(job)

	var samples []string
	totalFiles := 0
	walkErr := filepath.WalkDir(job.DownloadDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		totalFiles++
		if IsSample(d.Name()) {
			samples = append(samples, path)
		}
		return nil
	})
	if walkErr != nil {
		log.Warn("sample cleanup walk failed", "dir", job.DownloadDir, "err", walkErr)
		job.OutputLines = append(job.OutputLines,
			fmt.Sprintf("walk %s: %v", job.DownloadDir, walkErr))
		return nil
	}

	if len(samples) == 0 {
		log.Debug("no sample files found", "dir", job.DownloadDir)
		return nil
	}
	if len(samples) >= totalFiles {
		// Every file looks like a sample — refuse to wipe the whole
		// release. Matches Python's "false-positive" branch.
		log.Info("skipping sample cleanup: every file matches sample pattern",
			"dir", job.DownloadDir, "files", totalFiles)
		job.OutputLines = append(job.OutputLines,
			fmt.Sprintf("sample cleanup skipped: %d/%d files match (false positive guard)",
				len(samples), totalFiles))
		return nil
	}

	for _, p := range samples {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := os.Remove(p); err != nil {
			log.Warn("failed to remove sample", "path", p, "err", err)
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("remove %s: %v", p, err))
			continue
		}
		log.Info("removed sample file", "path", p)
		job.OutputLines = append(job.OutputLines, fmt.Sprintf("removed sample %s", p))
	}
	return nil
}

func (s *SampleCleanupStage) logger(job *Job) *slog.Logger {
	if s.Log != nil {
		return s.Log.With("job", job.Queue.ID, "stage", s.Name())
	}
	return slog.Default().With("job", job.Queue.ID, "stage", s.Name())
}
