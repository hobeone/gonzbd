package postproc

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/deobfuscate"
)

// DeobfuscateStage renames obfuscated files to human-readable names after extraction.
type DeobfuscateStage struct {
	// toggle provides the thread-safe SetEnabled/enabled flag.
	toggle
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewDeobfuscateStage constructs a DeobfuscateStage.
func NewDeobfuscateStage() *DeobfuscateStage {
	return &DeobfuscateStage{}
}

// Name returns the stage identifier.
func (*DeobfuscateStage) Name() string { return "deobfuscate" }

// Run invokes deobfuscate.Deobfuscate against job.DownloadDir.
func (d *DeobfuscateStage) Run(ctx context.Context, job *Job) error {
	if !d.enabled() {
		return nil
	}

	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "deobfuscate", "job", job.JobID())

	logf(ctx, log, job, slog.LevelInfo, "Starting deobfuscation in %s (useful name: %s)", job.DownloadDir, job.Name())

	renames, err := deobfuscate.Deobfuscate(ctx, log, job.DownloadDir, job.Name(), job.Sanitize)
	if len(renames) == 0 {
		logf(ctx, log, job, slog.LevelInfo, "No files needed deobfuscation")
	} else {
		logf(ctx, log, job, slog.LevelInfo, "Deobfuscated %d file(s)", len(renames))
		for _, r := range renames {
			markRenamed(job, r.From, r.To)
			logf(ctx, log, job, slog.LevelInfo, "%s → %s", filepath.Base(r.From), filepath.Base(r.To))
		}
	}

	// Subtitle alignment: rename .srt files to match the dominant video.
	subRenames, subErr := deobfuscate.Subtitles(log, job.DownloadDir)
	if len(subRenames) > 0 {
		logf(ctx, log, job, slog.LevelInfo, "Renamed %d subtitle file(s)", len(subRenames))
		for _, r := range subRenames {
			markRenamed(job, r.From, r.To)
			logf(ctx, log, job, slog.LevelInfo, "%s → %s", filepath.Base(r.From), filepath.Base(r.To))
		}
	}
	// Prefer to report the primary deobfuscation error if both failed.
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "Error: deobfuscation failed: %v", err)
		return fmt.Errorf("deobfuscate: %w", err)
	}
	if subErr != nil {
		logf(ctx, log, job, slog.LevelWarn, "Error: subtitle alignment failed: %v", subErr)
		return fmt.Errorf("deobfuscate subtitles: %w", subErr)
	}
	return nil
}

// ScriptStage invokes the user's post-processing script (if any). A job
// with Script == "" or Script == "None" is skipped (matching Python).
