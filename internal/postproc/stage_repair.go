package postproc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/par2"
)

// RepairStage verifies and repairs extracted files using par2 recovery data.
type RepairStage struct {
	mu sync.RWMutex
	// Par2Opts configures the par2 binary path and turbo mode.
	Par2Opts par2.RunOptions
	// ParseOpts defines safety limits for PAR2 parsing. Applied only at stage
	// construction; unlike the other fields it is not runtime-mutable (Apply
	// deliberately leaves it untouched).
	ParseOpts par2.ParseOptions
	// UseGoPar2 enables the native par2engine library for verification
	// and repair. When true and native repair fails, falls back to the
	// external par2 binary if available (subject to GoPar2Fallback).
	UseGoPar2 bool
	// GoPar2Fallback allows a failed native par2 repair to retry with the
	// external par2 binary. Only relevant when UseGoPar2 is true.
	// Default true.
	GoPar2Fallback bool
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// RepairConfig is the full set of runtime-mutable RepairStage settings, applied
// atomically by Apply. Like UnpackConfig it uses the stage's own option types
// (par2.RunOptions), keeping internal/postproc free of internal/config; the
// caller owns the config→RepairConfig translation.
//
// ParseOpts is deliberately absent: it has no runtime setter and is applied
// only at stage construction, so Apply must not touch it.
type RepairConfig struct {
	// Par2 is the full par2 option set, including the construction-only Caps
	// pointer, which the caller carries forward.
	Par2           par2.RunOptions
	UseGoPar2      bool
	GoPar2Fallback bool
}

// Apply atomically replaces all runtime-mutable state from c under a single
// lock. It supersedes the former per-field Set* methods. ParseOpts is
// preserved (construction-only). Thread-safe; takes effect for the next job.
func (s *RepairStage) Apply(c RepairConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Par2Opts = c.Par2
	s.UseGoPar2 = c.UseGoPar2
	s.GoPar2Fallback = c.GoPar2Fallback
}

// NewRepairStage constructs a RepairStage with default settings.
func NewRepairStage() *RepairStage { return &RepairStage{} }

// NewRepairStageWith constructs a RepairStage with the given options.
func NewRepairStageWith(par2Opts par2.RunOptions) *RepairStage {
	return &RepairStage{Par2Opts: par2Opts}
}

// Name returns the stage identifier.
func (*RepairStage) Name() string { return "repair" }

// Run finds par2 sets in job.DownloadDir and repairs each. No-op when the
// job has no par2 files.
func (s *RepairStage) Run(ctx context.Context, job *Job) error {
	s.mu.RLock()
	par2Opts := s.Par2Opts
	parseOpts := s.ParseOpts
	useGoPar2Val := s.UseGoPar2
	goPar2FallbackVal := s.GoPar2Fallback
	s.mu.RUnlock()

	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "repair", "job", job.Queue.ID)

	// If Direct Unpack successfully extracted the archives during download,
	// we can skip PAR2 repair entirely — but only when QuickCheck has no
	// outstanding doubt. DirectUnpack only knows whether rarengine could
	// mechanically walk the archive's entries, not whether the source data
	// was complete and correct (see directunpack.DirectUnpacker.MarkCorrupt
	// for the matching defense on the DirectUnpack side), so it may stand in
	// for verification only where there is no verification to be had.
	//
	// QuickCheckInconclusive is the case this switch exists to keep separate
	// from QuickCheckNotRun. Both mean QuickCheck reported no damage, and
	// only one of them means QuickCheck looked (#294).
	directUnpackClean := len(job.DirectUnpackSets) > 0 &&
		len(job.DirectUnpackFailures) == 0 && len(job.DirectUnpackSkipped) == 0
	switch job.QuickCheck {
	case QuickCheckNotRun:
		if directUnpackClean {
			logf(ctx, log, job, slog.LevelInfo, "[repair] Skipped: Direct Unpack successfully extracted all archives during download")
			return nil
		}
	case QuickCheckClean:
		// Verification already confirmed every par2-tracked CRC, so the
		// expensive par2 verify+repair subprocess buys nothing. Saves 10-30s
		// per healthy job; par2 cleanup is the separate par2_cleanup stage
		// that runs after unpack.
		logf(ctx, log, job, slog.LevelInfo, "[repair] Skipped: QuickCheck already verified all file CRCs")
		return nil
	case QuickCheckDamaged, QuickCheckInconclusive:
		// Repair runs. Damaged has a verdict to act on; Inconclusive has
		// none, which is the reason to look rather than a reason not to.
		logf(ctx, log, job, slog.LevelInfo, "[repair] Running: QuickCheck reported %s", job.QuickCheck)
	}

	logf(ctx, log, job, slog.LevelInfo, "Scanning for par2 files in %s", job.DownloadDir)

	sets, err := par2.FindPar2Files(job.DownloadDir, parseOpts)
	if err != nil {
		job.ParError = true
		return fmt.Errorf("repair: find par2 sets: %w", err)
	}

	vs := NewVerifiedSets(job.DownloadDir, log)
	if vs.AllVerified() {
		logf(ctx, log, job, slog.LevelInfo, "[repair] All sets previously verified — skipping")
		return nil
	}

	var firstErr error
	if len(sets) > 0 {
		logf(ctx, log, job, slog.LevelInfo, "Found %d par2 set(s)", len(sets))

		// Collect all non-par2 files in the download directory to pass as
		// extra arguments. This lets par2 checksum-match files even when
		// their names don't match the par2 set's expectations (e.g.
		// obfuscated or renamed files).
		dataFiles, scanErr := listNonPar2Files(job.DownloadDir)
		if scanErr != nil {
			job.ParError = true
			return fmt.Errorf("repair: scan data files: %w", scanErr)
		}
		logf(ctx, log, job, slog.LevelInfo, "Found %d non-par2 data file(s) for checksum matching", len(dataFiles))

		for _, set := range sets {
			if err := s.processPar2Set(ctx, log, job, set, dataFiles, par2Opts, vs, useGoPar2Val, goPar2FallbackVal); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	} else {
		logf(ctx, log, job, slog.LevelInfo, "No par2 files found")
	}

	return firstErr
}

// processPar2Set processes a single par2 set: checks verification status, dispatches the repair tool,
// captures tool output, and handles repair failure/success records.
func (s *RepairStage) processPar2Set(
	ctx context.Context,
	log *slog.Logger,
	job *Job,
	set par2.Set,
	dataFiles []string,
	par2Opts par2.RunOptions,
	vs *VerifiedSets,
	useGoPar2Val, goPar2FallbackVal bool,
) error {
	main := set.ParseFile()
	if main == "" {
		logf(ctx, log, job, slog.LevelInfo, "Skipped par2 set %q: no main file", set.Name)
		return nil
	}
	if vs.IsVerified(set.Name) {
		logf(ctx, log, job, slog.LevelInfo, "[repair] Skipping previously verified set: %s", set.Name)
		return nil
	}

	repairOpts := par2Opts
	if repairOpts.Sandbox.TargetDir == "" {
		repairOpts.Sandbox.TargetDir = job.DownloadDir
	}
	repairOpts.OnLine = func(line string) {
		if job.OnOutput != nil {
			job.OnOutput("par2", line)
		}
	}
	repairOpts.OnCommand = func(cmdLine string) {
		logf(ctx, log, job, slog.LevelInfo, "Running: %s", cmdLine)
	}

	if cErr := fsutil.CheckContainment(job.DownloadDir); cErr != nil {
		job.ParError = true
		vs.MarkVerified(set.Name, false)
		logf(ctx, log, job, slog.LevelWarn, "Error: pre-repair containment violation for %q: %v", set.Name, cErr)
		return fmt.Errorf("repair %q: pre-repair containment check: %w", set.Name, cErr)
	}

	// Dispatch: native par2engine vs external par2 binary.
	res, err := dispatchRepairTool(ctx, log, job, main, dataFiles, repairOpts, useGoPar2Val, goPar2FallbackVal)

	// Capture par2 tool output for the stage log.
	job.OutputLines = append(job.OutputLines, fmt.Sprintf("[par2] %s", set.Name))
	if res.CommandLine != "" {
		job.OutputLines = append(job.OutputLines, "Command: "+res.CommandLine)
	}
	if res.Output != "" {
		job.OutputLines = append(job.OutputLines, toolOutputLines(res.Output)...)
	}

	return s.handleRepairResult(ctx, log, job, set, vs, res, err)
}

// handleRepairResult evaluates the repair result: classifies errors, records failures,
// or registers successful repairs in job state.
func (s *RepairStage) handleRepairResult(
	ctx context.Context,
	log *slog.Logger,
	job *Job,
	set par2.Set,
	vs *VerifiedSets,
	res par2.RepairResult,
	err error,
) error {
	if cErr := fsutil.CheckContainment(job.DownloadDir); cErr != nil {
		job.ParError = true
		vs.MarkVerified(set.Name, false)
		logf(ctx, log, job, slog.LevelWarn, "Error: containment violation after par2 repair %q: %v", set.Name, cErr)
		return fmt.Errorf("repair %q: containment check: %w", set.Name, cErr)
	}

	if err != nil {
		job.ParError = true
		vs.MarkVerified(set.Name, false)
		logf(ctx, log, job, slog.LevelWarn, "Error: par2 repair %q failed: %v", set.Name, err)
		return fmt.Errorf("repair %q: %w", set.Name, err)
	}

	if !res.Success {
		job.ParError = true
		vs.MarkVerified(set.Name, false)

		// I3: Not enough recovery blocks
		if res.NeedMoreBlocks {
			job.NeedRequeue = true
			job.RequeueBlocksNeeded = res.BlocksNeeded
			job.RequeueReason = fmt.Sprintf(
				"par2 needs %d more recovery blocks for %q",
				res.BlocksNeeded, set.Name)
			logf(ctx, log, job, slog.LevelWarn,
				"Par2 repair %q needs %d more blocks — repair not possible with current data",
				set.Name, res.BlocksNeeded)
			return fmt.Errorf("repair %q: need %d more recovery blocks", set.Name, res.BlocksNeeded)
		}

		// I4: Main par2 file corrupt/missing
		if res.Parsed != nil && res.Parsed.Status == par2.StatusInvalidPar2 {
			job.NeedRequeue = true
			job.RequeueReason = fmt.Sprintf(
				"par2 main file corrupt/missing for %q — re-download needed",
				set.Name)
			logf(ctx, log, job, slog.LevelWarn,
				"Par2 main file corrupt/missing for %q — repair not possible",
				set.Name)
			return fmt.Errorf("repair %q: main par2 file corrupt or missing", set.Name)
		}

		logf(ctx, log, job, slog.LevelWarn, "Error: par2 repair %q unsuccessful (exit=%d)", set.Name, res.ExitCode)
		return fmt.Errorf("repair %q: unsuccessful (exit=%d)", set.Name, res.ExitCode)
	}

	recordRepairSuccess(ctx, log, set, job, vs, res)
	return nil
}

// dispatchRepairTool selects and runs either the native Go par2engine or the
// external par2 binary, with optional fallback. It mirrors the
// GoUnRAR/UnRAR dispatch pattern in stage_unpack.go.
func dispatchRepairTool(
	ctx context.Context,
	log *slog.Logger,
	job *Job,
	main string,
	dataFiles []string,
	repairOpts par2.RunOptions,
	useGoPar2, fallback bool,
) (par2.RepairResult, error) {
	if !useGoPar2 {
		par2Bin := repairOpts.Bin()
		if _, lookErr := exec.LookPath(par2Bin); lookErr != nil {
			useGoPar2 = true // external not found, fall back to native
			logf(ctx, log, job, slog.LevelInfo, "%s not found in PATH, falling back to go_par2", par2Bin)
		}
	}

	if !useGoPar2 {
		return par2.RepairWith(ctx, repairOpts, main, dataFiles...)
	}

	logf(ctx, log, job, slog.LevelInfo, "Using go_par2 for PAR2 (native Go)")
	// onLine feeds job.OnOutput (UI/history) only — go_par2's teeHandler
	// already writes each engine log record to the structured log via its
	// embedded base handler, so re-logging here would duplicate every line.
	res, err := par2.GoRepair(ctx, log, main, job.DownloadDir, func(line string) {
		if job.OnOutput != nil {
			job.OnOutput("par2", line)
		}
	})

	// Fallback: retry with the external par2 binary only when the native result
	// is inconclusive. A "not enough recovery data" verdict (NeedMoreBlocks) is
	// authoritative — external par2 re-scans the same content-matched files and
	// reaches the identical conclusion, so retrying only burns a full scan
	// without changing the outcome. Gated on fallback so users can disable the
	// retry entirely.
	if fallback && shouldFallbackToExternal(res, err) {
		par2Bin := repairOpts.Bin()
		if _, lookErr := exec.LookPath(par2Bin); lookErr == nil {
			reason := nativeRepairReason(res, err)
			logf(ctx, log, job, slog.LevelWarn,
				"go_par2 result inconclusive (%s), retrying with external %s", reason, par2Bin)
			if job.OnOutput != nil {
				job.OnOutput("go_par2",
					fmt.Sprintf("Native repair result: %s — retrying with external %s", reason, par2Bin))
			}
			return par2.RepairWith(ctx, repairOpts, main, dataFiles...)
		}
	}
	return res, err
}

// shouldFallbackToExternal reports whether a native go_par2 result warrants a
// retry with the external par2 binary. GoRepair returns err=nil for a definitive
// logical verdict, so the decision cannot key on err alone.
//
// The only definitive go_par2 verdict is "not enough recovery data"
// (NeedMoreBlocks): a deterministic Reed-Solomon shard count that the external
// binary must agree with, so retrying it only burns a redundant scan. Every
// other non-success — including a go_par2 decoder/parse failure (err != nil) —
// falls back, because a parse failure is ambiguous: the mature external par2
// may read a par2 file go_par2 could not, and if it also fails,
// handleRepairResult requeues for re-download.
func shouldFallbackToExternal(res par2.RepairResult, err error) bool {
	if err != nil {
		return true
	}
	if res.Success {
		return false
	}
	if res.NeedMoreBlocks {
		return false
	}
	return true
}

// nativeRepairReason describes why a native go_par2 result triggered a fallback,
// for logging. It prefers the engine error, then a specific block count, then a
// generic exit-code summary.
func nativeRepairReason(res par2.RepairResult, err error) string {
	if err != nil {
		return err.Error()
	}
	if res.NeedMoreBlocks {
		return fmt.Sprintf("needs %d more recovery blocks", res.BlocksNeeded)
	}
	return fmt.Sprintf("unsuccessful (exit=%d)", res.ExitCode)
}

// recordRepairSuccess updates job state after a successful par2 repair:
// marks the set verified, records consumed par2 files, wires renames for
// downstream deobfuscation, and protects joinables and repair sources from
// premature cleanup deletion.
func recordRepairSuccess(ctx context.Context, log *slog.Logger, set par2.Set, job *Job, vs *VerifiedSets, res par2.RepairResult) {
	vs.MarkVerified(set.Name, true)
	logf(ctx, log, job, slog.LevelInfo, "Par2 repair %q succeeded", set.Name)

	// M7: record par2 files as consumed for cleanup protection.
	if job.ConsumedFiles == nil {
		job.ConsumedFiles = make(map[string]struct{})
	}
	if set.MainFile != "" {
		job.ConsumedFiles[set.MainFile] = struct{}{}
	}
	for _, ef := range set.ExtraFiles {
		job.ConsumedFiles[ef] = struct{}{}
	}

	if res.Parsed == nil {
		return
	}

	// I1: Wire par2 renames to Job for downstream deobfuscation.
	// Matches SABnzbd's nzo.renamed_file(renames) in par2cmdline_verify.
	if len(res.Parsed.Renames) > 0 {
		if job.Par2Renames == nil {
			job.Par2Renames = make(map[string]string)
		}
		for canonical, onDisk := range res.Parsed.Renames {
			job.Par2Renames[canonical] = onDisk
			logf(ctx, log, job, slog.LevelInfo, "Par2 rename: %q → %q", onDisk, canonical)
		}
	}

	// I2: Wire used_joinables and used_for_repair into ConsumedFiles to
	// prevent premature cleanup deletion. Matches SABnzbd's
	// used_joinables/used_for_repair tracking.
	for _, jf := range res.Parsed.UsedJoinables {
		job.ConsumedFiles[filepath.Join(job.DownloadDir, jf)] = struct{}{}
	}
	for _, rf := range res.Parsed.UsedForRepair {
		job.ConsumedFiles[filepath.Join(job.DownloadDir, rf)] = struct{}{}
	}
	if n := len(res.Parsed.UsedJoinables) + len(res.Parsed.UsedForRepair); n > 0 {
		logf(ctx, log, job, slog.LevelInfo,
			"Par2 consumed %d joinable(s) + %d repair source(s) → protected from cleanup",
			len(res.Parsed.UsedJoinables), len(res.Parsed.UsedForRepair))
	}
}

// par2BackupRe matches files that par2 creates as backups during repair:
// a known archive/data extension followed by ".N" (e.g. ".rar.1", ".mkv.2").
var par2BackupRe = regexp.MustCompile(`(?i)\.(rar|r\d+|7z|zip|mkv|avi|mp4|flac|mp3|srt|nfo)\.\d{1,3}$`)

// cleanupPar2Backups removes backup files created by par2 during repair.
// When par2 repairs a damaged file "movie.rar", it renames the damaged
// original to "movie.rar.1" and writes the repaired data to "movie.rar".
// This function finds and removes those ".N" backup files, but only when
// the corresponding repaired file exists (safety check).
func cleanupPar2Backups(dir string, log *slog.Logger) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	// Build a set of existing filenames for the safety check.
	existing := make(map[string]bool, len(entries))
	for _, e := range entries {
		existing[e.Name()] = true
	}

	var removed []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !par2BackupRe.MatchString(name) {
			continue
		}
		// Extract the original filename by stripping the trailing ".N" suffix.
		// e.g. "movie.part01.rar.1" → "movie.part01.rar"
		lastDot := strings.LastIndex(name, ".")
		if lastDot <= 0 {
			continue
		}
		originalName := name[:lastDot]
		// Only delete if the repaired original exists alongside the backup.
		if !existing[originalName] {
			log.Info("repair: keeping par2 backup (repaired file missing)",
				"backup", name, "expected", originalName)
			continue
		}
		path := filepath.Join(dir, name)
		if err := fsutil.Remove(path); err != nil {
			log.Warn("repair: failed to remove par2 backup", "file", name, "err", err)
			continue
		}
		log.Info("repair: removed par2 backup", "file", name)
		removed = append(removed, name)
	}
	return removed
}

// listNonPar2Files returns the absolute paths of all non-directory,
// non-par2 files in dir. These paths are passed to par2 as extra data
// files for checksum matching.
func listNonPar2Files(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".par2") {
			continue
		}
		result = append(result, filepath.Join(dir, e.Name()))
	}
	return result, nil
}
