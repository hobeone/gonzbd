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

	"github.com/hobeone/gonzbd/internal/par2"
)

type RepairStage struct {
	// Par2Opts configures the par2 binary path and turbo mode.
	Par2Opts par2.RunOptions
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
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "postproc/repair", "job", job.Queue.ID)

	logf(log, job, slog.LevelInfo, "Scanning for par2 files in %s", job.DownloadDir)

	// If QuickCheck already confirmed all files have matching CRCs, skip
	// the expensive par2 verify+repair subprocess. This saves 10-30s per
	// healthy job. Par2 cleanup is handled by the separate par2_cleanup
	// stage that runs after unpack.
	if job.QuickCheckPassed {
		logf(log, job, slog.LevelInfo, "QuickCheck verified all CRCs — skipping par2 repair")
		job.OutputLines = append(job.OutputLines,
			"[repair] Skipped: QuickCheck already verified all file CRCs")
		return nil
	}

	sets, err := par2.FindPar2Files(job.DownloadDir)
	if err != nil {
		job.ParError = true
		return fmt.Errorf("repair: find par2 sets: %w", err)
	}

	vs := NewVerifiedSets(job.DownloadDir)
	if vs.AllVerified() {
		logf(log, job, slog.LevelInfo, "All par2 sets previously verified, skipping repair")
		job.OutputLines = append(job.OutputLines, "[repair] All sets previously verified — skipping")
		return nil
	}

	var firstErr error
	if len(sets) > 0 {
		logf(log, job, slog.LevelInfo, "Found %d par2 set(s)", len(sets))

		// Collect all non-par2 files in the download directory to pass as
		// extra arguments. This lets par2 checksum-match files even when
		// their names don't match the par2 set's expectations (e.g.
		// obfuscated or renamed files).
		dataFiles, scanErr := listNonPar2Files(job.DownloadDir)
		if scanErr != nil {
			job.ParError = true
			return fmt.Errorf("repair: scan data files: %w", scanErr)
		}
		logf(log, job, slog.LevelInfo, "Found %d non-par2 data file(s) for checksum matching", len(dataFiles))

		for _, set := range sets {
			main := set.MainFile
			if main == "" && len(set.ExtraFiles) > 0 {
				main = set.ExtraFiles[0]
			}
			if main == "" {
				logf(log, job, slog.LevelInfo, "Skipped par2 set %q: no main file", set.Name)
				continue
			}
			if vs.IsVerified(set.Name) {
				logf(log, job, slog.LevelInfo, "Skipping previously verified set %q", set.Name)
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("[repair] Skipping previously verified set: %s", set.Name))
				continue
			}
			repairOpts := s.Par2Opts
			repairOpts.OnLine = func(line string) {
				if job.OnOutput != nil {
					job.OnOutput("par2", line)
				}
			}
			repairOpts.OnCommand = func(cmdLine string) {
				logf(log, job, slog.LevelInfo, "Running: %s", cmdLine)
			}

			// Dispatch: native par2engine vs external par2 binary.
			// Follows the same pattern as GoUnRAR/UnRAR in stage_unpack.go.
			var res par2.RepairResult
			useGoPar2 := s.UseGoPar2

			if !useGoPar2 {
				// Check if external par2 binary is available.
				par2Bin := repairOpts.Command
				if par2Bin == "" {
					par2Bin = "par2"
				}
				if _, lookErr := exec.LookPath(par2Bin); lookErr != nil {
					useGoPar2 = true // external not found, fall back to native
					logf(log, job, slog.LevelInfo, "%s not found in PATH, falling back to go_par2", par2Bin)
				}
			}

			if useGoPar2 {
				logf(log, job, slog.LevelInfo, "Using go_par2 for PAR2 (native Go)")
				res, err = par2.GoRepair(ctx, log, main, job.DownloadDir, func(line string) {
					logf(log, job, slog.LevelInfo, "%s", line)
				})

				// Fallback: if native repair failed or returned a non-success
				// result, retry with the external par2 binary if available.
				// GoRepair returns err=nil for logical failures like
				// insufficient blocks (NeedMoreBlocks=true) — the fallback
				// must check both err and !res.Success so those cases also
				// reach the external tool, which may succeed where the Go
				// library gave a false negative.
				// Gated on GoPar2Fallback so users can disable the retry.
				if (err != nil || !res.Success) && s.GoPar2Fallback {
					par2Bin := repairOpts.Command
					if par2Bin == "" {
						par2Bin = "par2"
					}
					if _, lookErr := exec.LookPath(par2Bin); lookErr == nil {
						reason := "non-success result"
						if err != nil {
							reason = err.Error()
						}
						logf(log, job, slog.LevelWarn,
							"go_par2 result not successful (%s), retrying with external %s", reason, par2Bin)
						if job.OnOutput != nil {
							job.OnOutput("go_par2",
								fmt.Sprintf("Native repair result: %s — retrying with external %s", reason, par2Bin))
						}
						res, err = par2.RepairWith(ctx, repairOpts, main, dataFiles...)
					}
				}
			} else {
				res, err = par2.RepairWith(ctx, repairOpts, main, dataFiles...)
			}
			// Capture par2 tool output for the stage log.
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[par2] %s", set.Name))
			if res.CommandLine != "" {
				job.OutputLines = append(job.OutputLines, "Command: "+res.CommandLine)
			}
			if res.Output != "" {
				job.OutputLines = append(job.OutputLines,
					toolOutputLines(res.Output)...)
			}
			if err != nil {
				job.ParError = true
				vs.MarkVerified(set.Name, false)
				logf(log, job, slog.LevelWarn, "Error: par2 repair %q failed: %v", set.Name, err)
				if firstErr == nil {
					firstErr = fmt.Errorf("repair %q: %w", set.Name, err)
				}
				continue
			}
			if !res.Success {
				// I3: Not enough recovery blocks — record for informational
				// purposes but treat as a repair failure so the pipeline
				// continues (unpack, finalize, script still run).
				if res.NeedMoreBlocks {
					job.NeedRequeue = true
					job.RequeueBlocksNeeded = res.BlocksNeeded
					job.RequeueReason = fmt.Sprintf(
						"par2 needs %d more recovery blocks for %q",
						res.BlocksNeeded, set.Name)
					logf(log, job, slog.LevelWarn,
						"Par2 repair %q needs %d more blocks — repair not possible with current data",
						set.Name, res.BlocksNeeded)
					job.ParError = true
					vs.MarkVerified(set.Name, false)
					if firstErr == nil {
						firstErr = fmt.Errorf("repair %q: need %d more recovery blocks", set.Name, res.BlocksNeeded)
					}
					continue
				}

				// I4: Main par2 file corrupt/missing — record and continue.
				if res.Parsed != nil && res.Parsed.Status == par2.StatusInvalidPar2 {
					job.NeedRequeue = true
					job.RequeueReason = fmt.Sprintf(
						"par2 main file corrupt/missing for %q — re-download needed",
						set.Name)
					logf(log, job, slog.LevelWarn,
						"Par2 main file corrupt/missing for %q — repair not possible",
						set.Name)
					job.ParError = true
					vs.MarkVerified(set.Name, false)
					if firstErr == nil {
						firstErr = fmt.Errorf("repair %q: main par2 file corrupt or missing", set.Name)
					}
					continue
				}

				job.ParError = true
				vs.MarkVerified(set.Name, false)
				logf(log, job, slog.LevelWarn, "Error: par2 repair %q unsuccessful (exit=%d)", set.Name, res.ExitCode)
				if firstErr == nil {
					firstErr = fmt.Errorf("repair %q: unsuccessful (exit=%d)", set.Name, res.ExitCode)
				}
			} else {
				vs.MarkVerified(set.Name, true)
				logf(log, job, slog.LevelInfo, "Par2 repair %q succeeded", set.Name)
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

				// I1: Wire par2 renames to Job for downstream deobfuscation.
				// Matches SABnzbd's nzo.renamed_file(renames) in par2cmdline_verify.
				if res.Parsed != nil && len(res.Parsed.Renames) > 0 {
					if job.Par2Renames == nil {
						job.Par2Renames = make(map[string]string)
					}
					for canonical, onDisk := range res.Parsed.Renames {
						job.Par2Renames[canonical] = onDisk
						logf(log, job, slog.LevelInfo, "Par2 rename: %q → %q", onDisk, canonical)
					}
				}

				// I2: Wire used_joinables and used_for_repair into
				// ConsumedFiles to prevent premature cleanup deletion.
				// Matches SABnzbd's used_joinables/used_for_repair tracking.
				if res.Parsed != nil {
					for _, jf := range res.Parsed.UsedJoinables {
						absPath := filepath.Join(job.DownloadDir, jf)
						job.ConsumedFiles[absPath] = struct{}{}
					}
					for _, rf := range res.Parsed.UsedForRepair {
						absPath := filepath.Join(job.DownloadDir, rf)
						job.ConsumedFiles[absPath] = struct{}{}
					}
					if n := len(res.Parsed.UsedJoinables) + len(res.Parsed.UsedForRepair); n > 0 {
						logf(log, job, slog.LevelInfo,
							"Par2 consumed %d joinable(s) + %d repair source(s) → protected from cleanup",
							len(res.Parsed.UsedJoinables), len(res.Parsed.UsedForRepair))
					}
				}
			}
		}
	} else {
		logf(log, job, slog.LevelInfo, "No par2 files found")
	}

	return firstErr
}

// par2BackupRe matches files that par2 creates as backups during repair:
// a known archive/data extension followed by ".N" (e.g. ".rar.1", ".mkv.2").
var par2BackupRe = regexp.MustCompile(`(?i)\.(rar|r\d+|7z|zip|mkv|avi|mp4|flac|mp3|srt|nfo)\.\d{1,3}$`)

// cleanupPar2Backups removes backup files created by par2 during repair.
// When par2 repairs a damaged file "movie.rar", it renames the damaged
// original to "movie.rar.1" and writes the repaired data to "movie.rar".
// This function finds and removes those ".N" backup files, but only when
// the corresponding repaired file exists (safety check).
func cleanupPar2Backups(dir string, log *slog.Logger) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	// Build a set of existing filenames for the safety check.
	existing := make(map[string]bool, len(entries))
	for _, e := range entries {
		existing[e.Name()] = true
	}

	var removed int
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
		if err := os.Remove(path); err != nil {
			log.Warn("repair: failed to remove par2 backup", "file", name, "err", err)
			continue
		}
		log.Info("repair: removed par2 backup", "file", name)
		removed++
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
