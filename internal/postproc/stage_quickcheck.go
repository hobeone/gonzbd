package postproc

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hobeone/gonzbd/internal/par2"
)

// QuickCheckStage relocates flat-downloaded files into the subdirectory structure expected by par2.
type QuickCheckStage struct {
	// toggle provides the thread-safe SetEnabled/enabled flag.
	toggle
	// ParseOpts defines safety limits for PAR2 parsing.
	ParseOpts par2.ParseOptions
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewQuickCheckStage constructs a QuickCheckStage with default settings.
func NewQuickCheckStage() *QuickCheckStage { return &QuickCheckStage{} }

// Name returns the stage identifier.
func (*QuickCheckStage) Name() string { return "quickcheck" }

// Run finds par2 sets and relocates flat files into their par2-specified
// subdirectory paths. After relocation, it verifies file integrity by
// comparing assembled CRC32 values (computed during download) against
// par2 manifest CRC32 values. Errors are non-fatal: par2 repair will
// independently report any files it cannot find or that are corrupted.
func (q *QuickCheckStage) Run(ctx context.Context, job *Job) error {
	log := q.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "quickcheck", "job", job.Queue.ID)

	if !q.enabled() {
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] Disabled — par2 repair will run the full verify/repair step")
		return nil
	}

	logf(ctx, log, job, slog.LevelInfo, "[quickcheck] Scanning for par2 files in %s", job.DownloadDir)

	sets, err := par2.FindPar2Files(job.DownloadDir, q.ParseOpts)
	if err != nil {
		// Inconclusive, not NotRun: the scan failed, so whether this job has
		// par2 sets is unknown. Claiming there was nothing to check would let
		// the repair stage's DirectUnpack shortcut skip par2 on the strength
		// of a question that was never answered.
		job.QuickCheck = QuickCheckInconclusive
		logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Failed to find par2 files: %v", err)
		return nil // non-fatal
	}
	if len(sets) == 0 {
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] No par2 files found — skipping subdirectory relocation and CRC verification")
		return nil
	}

	// Past this line the job has par2 sets, so QuickCheckNotRun — "there was
	// nothing to verify" — is no longer a true thing to leave behind. Claim
	// nothing by default and narrow to Clean or Damaged only where the work
	// was actually done (#314).
	//
	// This inverts which state is free. The zero value used to be the
	// permissive one, so every early return that forgot to assign handed the
	// repair stage consent to skip par2 — and one did: verifyJobCRCs' opening
	// guard, reachable only from here, so only ever for a job that has par2
	// sets. Now the conservative state is the one you get for free and the
	// permissive ones require saying so, which makes the next early return
	// added below fail safe by construction rather than by review.
	job.QuickCheck = QuickCheckInconclusive

	logf(ctx, log, job, slog.LevelInfo, "[quickcheck] Found %d par2 set(s), checking for subdirectory entries", len(sets))

	renames, err := par2.QuickCheckWithOptions(job.DownloadDir, sets, log, q.ParseOpts)
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Error: %v", err)
		return nil // non-fatal; the outcome set above already says why
	}

	if len(renames) > 0 {
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] Relocated %d file(s) into subdirectories", len(renames))
		for _, r := range renames {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[quickcheck] %s → %s", r.From, r.To))
		}
	} else {
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] No files needed subdirectory relocation")
	}

	return q.verifyJobCRCs(ctx, log, job, sets)
}

func (q *QuickCheckStage) verifyJobCRCs(ctx context.Context, log *slog.Logger, job *Job, sets []par2.Set) error {
	// No manifest, or one describing no files, so there are no expected CRCs
	// to compare the par2 sets against. The caller has already established
	// that sets is non-empty — it returns at len(sets) == 0 — so this leaves
	// QuickCheckInconclusive, set there. It used to leave the zero value,
	// which told the repair stage this job had nothing worth verifying while
	// its par2 sets went unchecked (#314).
	if job.Queue == nil || job.Queue.NumFiles() == 0 {
		logf(ctx, log, job, slog.LevelWarn,
			"[quickcheck] No manifest files to verify against, though %d par2 set(s) are present — par2 repair will run", len(sets))
		return nil
	}

	// Unlike the file listing, this must not degrade quietly: a job whose
	// manifest is unreadable would otherwise be reported as CRC-verified
	// having been checked against nothing. Returning an error records the
	// failure in the stage log and surfaces it in the history entry; the
	// runner deliberately does not abort the pipeline on a stage error, so
	// par2 repair still gets its turn.
	//
	// Leaving Inconclusive in place is what makes that last clause true. The
	// error return alone protects this stage's own claim, but the repair stage
	// reads the outcome, not the error — and under the old boolean pair
	// "could not verify" was indistinguishable from "had nothing to verify",
	// so DirectUnpack's success would skip par2 for a job nothing had
	// checked (#294).
	m, mErr := job.Queue.Manifest()
	if mErr != nil {
		return fmt.Errorf("quickcheck: cannot verify CRCs without the manifest: %w", mErr)
	}

	p := job.Queue.Progress()
	var assembledFiles []par2.AssembledFile
	for fi := range m.NumFiles() {
		name := m.FileSubject(fi)
		if fn := p.FileFilename(fi); fn != "" {
			name = fn
		}
		assembledFiles = append(assembledFiles, par2.AssembledFile{
			FileName: name,
			CRC32:    p.FileAssembledCRC32(fi),
			FileSize: m.FileBytes(fi),
		})
	}

	crcResult := par2.VerifyCRCsWithOptions(assembledFiles, sets, log, q.ParseOpts)
	unverifiable := crcResult.NoCRC + crcResult.Unverified + crcResult.Mismatched

	if crcResult.Checked > 0 {
		logf(ctx, log, job, slog.LevelInfo,
			"[quickcheck] CRC verification: %d/%d par2-tracked files verified OK",
			crcResult.Matched, crcResult.Checked+crcResult.NoCRC)

		for _, f := range crcResult.Files {
			if f.Match {
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("[quickcheck] ✓ %s: CRC verified (%08x)", f.FileName, f.AssembledCRC))
			}
		}

		if crcResult.Mismatched > 0 {
			logf(ctx, log, job, slog.LevelWarn,
				"[quickcheck] CRC MISMATCH detected — %d file(s) corrupted",
				crcResult.Mismatched)
			for _, f := range crcResult.Files {
				if !f.Match {
					job.OutputLines = append(job.OutputLines,
						fmt.Sprintf("[quickcheck] ✗ %s: CRC mismatch (assembled=%08x par2=%08x)",
							f.FileName, f.AssembledCRC, f.Par2CRC))
				}
			}
		}
	}

	if crcResult.NoCRC > 0 {
		for _, name := range crcResult.NoCRCFiles {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[quickcheck] ⚠ %s: CRC unavailable, verifying with par2", name))
		}
	}

	if crcResult.Unverified > 0 {
		logf(ctx, log, job, slog.LevelWarn,
			"[quickcheck] %d par2-tracked file(s) not found by name",
			crcResult.Unverified)
		for _, name := range crcResult.UnverifiedFiles {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[quickcheck] ⚠ %s: par2-tracked file not found by name", name))
		}
	}

	switch {
	case unverifiable > 0:
		job.QuickCheck = QuickCheckDamaged
		logf(ctx, log, job, slog.LevelInfo,
			"[quickcheck] %d/%d par2-tracked files verified OK, %d file(s) need par2 verification — repair stage will run",
			crcResult.Matched, crcResult.Matched+unverifiable, unverifiable)
	case crcResult.Checked > 0:
		job.QuickCheck = QuickCheckClean
		logf(ctx, log, job, slog.LevelInfo,
			"[quickcheck] All %d par2-tracked files verified OK — skipping par2 repair",
			crcResult.Matched)
	default:
		// Par2 sets were found but no assembled CRC was available to compare
		// against any of them, so nothing was actually verified. Stays at the
		// Inconclusive the caller set, which also keeps the pre-enum
		// behaviour: the old pair recorded Ran-and-not-Passed here, forcing
		// repair.
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] No CRC data available — par2 repair will run")
	}
	return nil
}

// RepairStage runs par2 verify+repair against every par2 set it finds in
// the job's DownloadDir. A set with status RepairNotPossible or an exec
// failure sets job.ParError; the pipeline continues (unpack may still
// succeed on an intact archive).
