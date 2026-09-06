package postproc

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/deobfuscate"
)

// RecoverPar2NamesStage restores original filenames from par2 manifests after extraction.
type RecoverPar2NamesStage struct {
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewRecoverPar2NamesStage constructs a RecoverPar2NamesStage.
func NewRecoverPar2NamesStage() *RecoverPar2NamesStage { return &RecoverPar2NamesStage{} }

// Name returns the stage identifier.
func (*RecoverPar2NamesStage) Name() string { return "recover_par2_names" }

// Run scans job.DownloadDir for par2 files and renames any files whose
// 16K-MD5 matches a par2-recorded filename.
func (r *RecoverPar2NamesStage) Run(ctx context.Context, job *Job) error {
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "recover_par2_names", "job", job.JobID())

	logf(ctx, log, job, slog.LevelInfo, "Running par2-based filename recovery in %s", job.DownloadDir)

	root, err := os.OpenRoot(job.DownloadDir)
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "Warning: failed to open root: %v", err)
		return nil
	}
	defer root.Close() //nolint:errcheck // read-only close

	renames, err := deobfuscate.Par2Rename(ctx, log, root, job.DownloadDir, job.Sanitize)
	if len(renames) == 0 {
		logf(ctx, log, job, slog.LevelInfo, "No par2-based renames needed")
	} else {
		logf(ctx, log, job, slog.LevelInfo, "Par2 renamed %d file(s)", len(renames))
		for _, ren := range renames {
			markRenamed(job, ren.From, ren.To)
			from := filepath.Base(ren.From)
			to := filepath.Base(ren.To)
			if ren.TrueName != "" && to != ren.TrueName {
				// The par2-recorded target already existed; GetUniqueFilename added a
				// numeric suffix to avoid overwriting it. Log both names so it's clear
				// why the destination differs from what par2 recorded.
				log.Warn("par2 rename: target name already existed on disk",
					"obfuscated", from,
					"par2_target", ren.TrueName,
					"renamed_to", to,
					"note", "existing file was not overwritten",
				)
			} else {
				log.Info("par2 rename", "from", from, "to", to)
			}
		}
	}
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "Warning: par2 rename encountered errors: %v", err)
		// Non-fatal: heuristic deobfuscation or the user can fix things.
	}
	return nil
}

// DeobfuscateStage renames obfuscated files in place using the job's
// display name as the rename target. Scope matches the deobfuscate
// package — see its doc for the skipped Python behaviors.
