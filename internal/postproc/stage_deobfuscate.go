package postproc

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/hobeone/gonzbd/internal/deobfuscate"
)

type DeobfuscateStage struct {
	mu       sync.RWMutex
	disabled bool
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewDeobfuscateStage constructs a DeobfuscateStage.
func NewDeobfuscateStage() *DeobfuscateStage {
	return &DeobfuscateStage{}
}

// SetEnabled enables or disables filename deobfuscation at runtime. Thread-safe.
func (d *DeobfuscateStage) SetEnabled(v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disabled = !v
}

// Name returns the stage identifier.
func (*DeobfuscateStage) Name() string { return "deobfuscate" }

// Run invokes deobfuscate.Deobfuscate against job.DownloadDir.
func (d *DeobfuscateStage) Run(ctx context.Context, job *Job) error {
	d.mu.RLock()
	disabled := d.disabled
	d.mu.RUnlock()

	if disabled {
		return nil
	}

	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "postproc/deobfuscate", "job", job.Queue.ID)

	logf(log, job, slog.LevelInfo, "Starting deobfuscation in %s (useful name: %s)", job.DownloadDir, job.Queue.Name)

	renames, err := deobfuscate.Deobfuscate(ctx, log, job.DownloadDir, job.Queue.Name, job.Sanitize)
	if len(renames) == 0 {
		logf(log, job, slog.LevelInfo, "No files needed deobfuscation")
	} else {
		logf(log, job, slog.LevelInfo, "Deobfuscated %d file(s)", len(renames))
		for _, r := range renames {
			logf(log, job, slog.LevelInfo, "%s → %s", filepath.Base(r.From), filepath.Base(r.To))
		}
	}

	// Subtitle alignment: rename .srt files to match the dominant video.
	subRenames, subErr := deobfuscate.Subtitles(log, job.DownloadDir)
	if len(subRenames) > 0 {
		logf(log, job, slog.LevelInfo, "Renamed %d subtitle file(s)", len(subRenames))
		for _, r := range subRenames {
			logf(log, job, slog.LevelInfo, "%s → %s", filepath.Base(r.From), filepath.Base(r.To))
		}
	}
	// Prefer to report the primary deobfuscation error if both failed.
	if err != nil {
		logf(log, job, slog.LevelWarn, "Error: deobfuscation failed: %v", err)
		return fmt.Errorf("deobfuscate: %w", err)
	}
	if subErr != nil {
		logf(log, job, slog.LevelWarn, "Error: subtitle alignment failed: %v", subErr)
		return fmt.Errorf("deobfuscate subtitles: %w", subErr)
	}
	return nil
}

// ScriptStage invokes the user's post-processing script (if any). A job
// with Script == "" or Script == "None" is skipped (matching Python).
