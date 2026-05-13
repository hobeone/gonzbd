package postproc

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/deobfuscate"
)

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
func (r *RecoverPar2NamesStage) Run(_ context.Context, job *Job) error {
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "postproc/recover_par2_names", "job", job.Queue.ID)

	logf(log, job, slog.LevelInfo, "Running par2-based filename recovery in %s", job.DownloadDir)

	renames, err := deobfuscate.Par2Rename(log, job.DownloadDir, job.Sanitize)
	if len(renames) == 0 {
		logf(log, job, slog.LevelInfo, "No par2-based renames needed")
	} else {
		logf(log, job, slog.LevelInfo, "Par2 renamed %d file(s)", len(renames))
		for _, ren := range renames {
			logf(log, job, slog.LevelInfo, "%s → %s", filepath.Base(ren.From), filepath.Base(ren.To))
		}
	}
	if err != nil {
		logf(log, job, slog.LevelWarn, "Warning: par2 rename encountered errors: %v", err)
		// Non-fatal: heuristic deobfuscation or the user can fix things.
	}
	return nil
}

// DeobfuscateStage renames obfuscated files in place using the job's
// display name as the rename target. Scope matches the deobfuscate
// package — see its doc for the skipped Python behaviors.
