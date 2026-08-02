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
		logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Failed to find par2 files: %v", err)
		return nil // non-fatal
	}
	if len(sets) == 0 {
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] No par2 files found — skipping subdirectory relocation and CRC verification")
		return nil
	}

	logf(ctx, log, job, slog.LevelInfo, "[quickcheck] Found %d par2 set(s), checking for subdirectory entries", len(sets))

	renames, err := par2.QuickCheckWithOptions(job.DownloadDir, sets, log, q.ParseOpts)
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Error: %v", err)
		return nil // non-fatal
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

	q.verifyJobCRCs(ctx, log, job, sets)
	return nil
}

func (q *QuickCheckStage) verifyJobCRCs(ctx context.Context, log *slog.Logger, job *Job, sets []par2.Set) {
	if job.Queue == nil || job.Queue.NumFiles() == 0 {
		return
	}
	job.QuickCheckRan = true

	m := job.Queue.Manifest()
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
				fmt.Sprintf("[quickcheck] ⚠ %s: download had failures, CRC unavailable", name))
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
		logf(ctx, log, job, slog.LevelInfo,
			"[quickcheck] %d/%d par2-tracked files verified OK, %d file(s) need par2 verification — repair stage will run",
			crcResult.Matched, crcResult.Matched+unverifiable, unverifiable)
	case crcResult.Checked > 0:
		logf(ctx, log, job, slog.LevelInfo,
			"[quickcheck] All %d par2-tracked files verified OK — skipping par2 repair",
			crcResult.Matched)
		job.QuickCheckPassed = true
	default:
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] No CRC data available — par2 repair will run")
	}
}

// RepairStage runs par2 verify+repair against every par2 set it finds in
// the job's DownloadDir. A set with status RepairNotPossible or an exec
// failure sets job.ParError; the pipeline continues (unpack may still
// succeed on an intact archive).
