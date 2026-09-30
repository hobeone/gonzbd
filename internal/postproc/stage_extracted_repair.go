package postproc

import (
	"context"
	"fmt"
	"log/slog"
)

// ExtractedRepairStage runs par2 verify+repair, once unpack has finished, on
// the par2 sets quickcheck deferred because they protect what an archive
// extracts to (Job.DeferredPar2Sets). It is the check on those files that the
// archive's own checksums cannot give: go_rar cannot check a BLAKE2sp-only or
// MAC digest, go_7z skips a member that records no CRC, and a stored archive
// passes damage through. It repairs with whatever recovery volumes are on
// disk, and a set it cannot verify or repair sets ParError, which fails the
// run. When the job held its recovery volumes back through download, the
// app's finalizer then retries it once with them released (#651).
//
// It uses Repair's configuration and its per-set repair, so the two stages
// cannot run par2 differently.
type ExtractedRepairStage struct {
	// Repair is the pipeline's repair stage.
	Repair *RepairStage
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewExtractedRepairStage constructs an ExtractedRepairStage that repairs with
// repair's configuration.
func NewExtractedRepairStage(repair *RepairStage) *ExtractedRepairStage {
	return &ExtractedRepairStage{Repair: repair}
}

// Name implements Stage.
func (*ExtractedRepairStage) Name() string { return "extracted_repair" }

// Run implements Stage. It does nothing for a job with no deferred par2 set,
// and skips one whose repair or unpack failed: the files it would check were
// then not extracted, and par2_cleanup keeps the par2 set.
func (s *ExtractedRepairStage) Run(ctx context.Context, job *Job) error {
	if len(job.DeferredPar2Sets) == 0 {
		return nil
	}
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "extracted_repair", "job", job.JobID())

	switch {
	case job.ParError:
		logf(ctx, log, job, slog.LevelInfo, "[extracted_repair] Skipped: repair failed, so nothing was extracted to verify")
		return nil
	case job.UnpackError:
		logf(ctx, log, job, slog.LevelInfo, "[extracted_repair] Skipped: unpack failed, so the extracted files are incomplete")
		return nil
	}

	logf(ctx, log, job, slog.LevelInfo, "[extracted_repair] Verifying %d par2 set(s) against the extracted files", len(job.DeferredPar2Sets))
	ran, err := s.Repair.repairSets(ctx, log, job, true)
	if err != nil {
		return err
	}
	if ran < len(job.DeferredPar2Sets) {
		// A deferred set no longer in the directory has checked nothing, and
		// the files it protects are exactly the ones nothing else checks.
		job.ParError = true
		return fmt.Errorf("extracted_repair: found %d of %d deferred par2 set(s)", ran, len(job.DeferredPar2Sets))
	}
	job.DeferredPar2Verified = true
	return nil
}
