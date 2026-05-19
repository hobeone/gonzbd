package postproc

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/hobeone/gonzbd/internal/par2"
)

type QuickCheckStage struct {
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
	// disabled is set atomically so SetEnabled can be called from any goroutine
	// (e.g. the API handler) while a job may be running in the postproc worker.
	disabled atomic.Bool
}

// NewQuickCheckStage constructs a QuickCheckStage with default settings.
func NewQuickCheckStage() *QuickCheckStage { return &QuickCheckStage{} }

// SetEnabled enables or disables the quick-check pass at runtime without
// requiring a server restart. Thread-safe; may be called from any goroutine.
func (q *QuickCheckStage) SetEnabled(enabled bool) { q.disabled.Store(!enabled) }

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
	log = log.With("component", "postproc/quickcheck", "job", job.Queue.ID)

	if q.disabled.Load() {
		logf(log, job, slog.LevelInfo, "quickcheck disabled — skipping CRC pre-verify, par2 repair will run")
		job.OutputLines = append(job.OutputLines,
			"[quickcheck] Disabled — par2 repair will run the full verify/repair step")
		return nil
	}

	logf(log, job, slog.LevelInfo, "Scanning for par2 files in %s", job.DownloadDir)

	sets, err := par2.FindPar2Files(job.DownloadDir)
	if err != nil {
		logf(log, job, slog.LevelWarn, "quickcheck: failed to find par2 files: %v", err)
		return nil // non-fatal
	}
	if len(sets) == 0 {
		logf(log, job, slog.LevelInfo, "quickcheck: no par2 files found, skipping")
		job.OutputLines = append(job.OutputLines,
			"[quickcheck] No par2 files found — skipping subdirectory relocation and CRC verification")
		return nil
	}

	logf(log, job, slog.LevelInfo, "quickcheck: found %d par2 set(s), checking for subdirectory entries", len(sets))
	job.OutputLines = append(job.OutputLines,
		fmt.Sprintf("[quickcheck] Found %d par2 set(s)", len(sets)))

	renames, err := par2.QuickCheck(job.DownloadDir, sets, log)
	if err != nil {
		logf(log, job, slog.LevelWarn, "quickcheck: %v", err)
		job.OutputLines = append(job.OutputLines,
			fmt.Sprintf("[quickcheck] Error: %v", err))
		return nil // non-fatal
	}

	if len(renames) > 0 {
		logf(log, job, slog.LevelInfo, "quickcheck: relocated %d file(s) into subdirectories", len(renames))
		for _, r := range renames {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[quickcheck] %s → %s", r.From, r.To))
		}
	} else {
		logf(log, job, slog.LevelInfo, "quickcheck: no files needed relocation")
		job.OutputLines = append(job.OutputLines,
			"[quickcheck] No files needed subdirectory relocation")
	}

	// CRC verification: compare assembled CRC32s (from download) against
	// par2 manifest CRC32s to detect corruption without re-reading files.
	if job.Queue != nil && len(job.Queue.Files) > 0 {
		var assembledFiles []par2.AssembledFile
		for _, jf := range job.Queue.Files {
			assembledFiles = append(assembledFiles, par2.AssembledFile{
				FileName: jf.Subject,
				CRC32:    jf.AssembledCRC32,
				FileSize: jf.Bytes,
			})
		}

		crcResult := par2.VerifyCRCs(assembledFiles, sets, log)

		// How many par2-tracked files could NOT be verified?
		unverifiable := crcResult.NoCRC + crcResult.Unverified + crcResult.Mismatched

		if crcResult.Checked > 0 {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[quickcheck] CRC verification: %d/%d par2-tracked files verified OK",
					crcResult.Matched, crcResult.Checked+crcResult.NoCRC))

			if crcResult.Mismatched > 0 {
				logf(log, job, slog.LevelWarn,
					"quickcheck: CRC MISMATCH detected — %d file(s) corrupted",
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

		// Report par2-tracked files with no CRC (download failures).
		if crcResult.NoCRC > 0 {
			for _, name := range crcResult.NoCRCFiles {
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("[quickcheck] ⚠ %s: download had failures, CRC unavailable", name))
			}
		}

		// Report par2 entries that couldn't be matched to any assembled file.
		if crcResult.Unverified > 0 {
			logf(log, job, slog.LevelWarn,
				"quickcheck: %d par2-tracked file(s) not found by name",
				crcResult.Unverified)
		}

		// Decision: can we skip par2 repair?
		switch {
		case unverifiable > 0:
			logf(log, job, slog.LevelInfo,
				"quickcheck: %d/%d par2-tracked files verified OK, %d could not be verified — par2 repair will run",
				crcResult.Matched, crcResult.Matched+unverifiable, unverifiable)
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[quickcheck] %d file(s) need par2 verification — repair stage will run",
					unverifiable))
		case crcResult.Checked > 0:
			logf(log, job, slog.LevelInfo,
				"quickcheck: all %d par2-tracked files verified OK — skipping par2 repair",
				crcResult.Matched)
			job.QuickCheckPassed = true
		default:
			logf(log, job, slog.LevelInfo,
				"quickcheck: no files could be CRC-verified — par2 repair will run")
			job.OutputLines = append(job.OutputLines,
				"[quickcheck] No CRC data available — par2 repair will run")
		}
	}

	return nil
}

// RepairStage runs par2 verify+repair against every par2 set it finds in
// the job's DownloadDir. A set with status RepairNotPossible or an exec
// failure sets job.ParError; the pipeline continues (unpack may still
// succeed on an intact archive).
