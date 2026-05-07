package postproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hobeone/gonzbd/internal/deobfuscate"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/sorting"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// RepairStage runs par2 verify+repair against every par2 set it finds in
// the job's DownloadDir. A set with status RepairNotPossible or an exec
// failure sets job.ParError; the pipeline continues (unpack may still
// succeed on an intact archive).
type RepairStage struct {
	// Par2Opts configures the par2 binary path and turbo mode.
	Par2Opts par2.RunOptions
	// Cleanup deletes all .par2 files after a successful repair.
	Cleanup bool
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewRepairStage constructs a RepairStage with default settings.
func NewRepairStage() *RepairStage { return &RepairStage{} }

// NewRepairStageWith constructs a RepairStage with the given options.
func NewRepairStageWith(par2Opts par2.RunOptions, cleanup bool) *RepairStage {
	return &RepairStage{Par2Opts: par2Opts, Cleanup: cleanup}
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

	sets, err := par2.FindPar2Files(job.DownloadDir)
	if err != nil {
		job.ParError = true
		return fmt.Errorf("repair: find par2 sets: %w", err)
	}

	var firstErr error
	repairSucceeded := true
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
			par2Bin := s.Par2Opts.Command
			if par2Bin == "" {
				par2Bin = "par2"
			}
			logf(log, job, slog.LevelInfo, "Running: %s r %s (+%d data files)", par2Bin, filepath.Base(main), len(dataFiles))
			res, err := par2.RepairWith(ctx, s.Par2Opts, main, dataFiles...)
			// Capture par2 tool output for the stage log.
			if res.Output != "" {
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("[par2] %s", set.Name))
				job.OutputLines = append(job.OutputLines,
					toolOutputLines(res.Output)...)
			}
			if err != nil {
				job.ParError = true
				repairSucceeded = false
				logf(log, job, slog.LevelWarn, "Error: par2 repair %q failed: %v", set.Name, err)
				if firstErr == nil {
					firstErr = fmt.Errorf("repair %q: %w", set.Name, err)
				}
				continue
			}
			if !res.Success {
				job.ParError = true
				repairSucceeded = false
				logf(log, job, slog.LevelWarn, "Error: par2 repair %q unsuccessful (exit=%d)", set.Name, res.ExitCode)
				if firstErr == nil {
					firstErr = fmt.Errorf("repair %q: unsuccessful (exit=%d)", set.Name, res.ExitCode)
				}
			} else {
				logf(log, job, slog.LevelInfo, "Par2 repair %q succeeded", set.Name)
			}
		}
	} else {
		logf(log, job, slog.LevelInfo, "No par2 files found")
	}

	// Par2 cleanup: delete par2 files after successful repair.
	if s.Cleanup && repairSucceeded {
		cleanupSets, err := par2.FindPar2Files(job.DownloadDir)
		if err == nil {
			var cleaned int
			for _, set := range cleanupSets {
				if set.MainFile != "" {
					_ = os.Remove(set.MainFile)
					cleaned++
				}
				for _, ef := range set.ExtraFiles {
					_ = os.Remove(ef)
					cleaned++
				}
			}
			if cleaned > 0 {
				logf(log, job, slog.LevelInfo, "Cleaned up %d par2 file(s)", cleaned)
			}
		}
	} else if s.Cleanup && !repairSucceeded {
		logf(log, job, slog.LevelInfo, "Keeping par2 files (repair failed)")
	}

	return firstErr
}

// UnpackStage extracts every archive it finds in job.DownloadDir,
// delegating to the unpack package's per-format functions.
//
// Destination is the same DownloadDir — extracted files land alongside
// the archives, matching Python's in-place unpack layout before the sort
// stage moves them. When Cleanup is true, source archive files are
// deleted after successful extraction (matching Python's PP bit 2).
type UnpackStage struct {
	// BaseOpts holds config-driven extraction options (tool paths, flags).
	// The job's password is merged at runtime.
	BaseOpts unpack.Options
	// Cleanup deletes source archive files after successful extraction.
	Cleanup bool
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewUnpackStage constructs an UnpackStage with default settings.
func NewUnpackStage() *UnpackStage { return &UnpackStage{} }

// NewUnpackStageWith constructs an UnpackStage with the given base options.
func NewUnpackStageWith(opts unpack.Options, cleanup bool) *UnpackStage {
	return &UnpackStage{BaseOpts: opts, Cleanup: cleanup}
}

// Name returns the stage identifier.
func (*UnpackStage) Name() string { return "unpack" }

// Run scans job.DownloadDir, routes each archive to the right unpack
// function, and captures any failures on job.UnpackError.
func (u *UnpackStage) Run(ctx context.Context, job *Job) error {
	log := u.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "postproc/unpack", "job", job.Queue.ID)

	// Skip extraction when repair has already failed — the archives are
	// corrupt and unpacking would produce garbage. Matches Python's
	// safe_postproc gate: "if all_ok: ... unpacker()".
	if job.ParError {
		logf(log, job, slog.LevelInfo, "Skipped: repair failed, archives may be corrupt")
		return nil
	}

	logf(log, job, slog.LevelInfo, "Scanning for archives in %s", job.DownloadDir)

	archives, err := unpack.Scan(job.DownloadDir)
	if err != nil {
		job.UnpackError = true
		return fmt.Errorf("unpack: scan: %w", err)
	}
	if len(archives) == 0 {
		logf(log, job, slog.LevelInfo, "No archives found")
		return nil
	}
	logf(log, job, slog.LevelInfo, "Found %d archive(s)", len(archives))
	for _, a := range archives {
		logf(log, job, slog.LevelInfo, "  %s: %s (%d parts)", archiveTypeName(a.Type), a.Name, len(a.Parts))
	}
	// Merge config-level options with per-job password.
	opts := u.BaseOpts
	opts.Password = job.Queue.Password
	var firstErr error
	// Track which archives extracted successfully for cleanup.
	var successfulArchives []unpack.Archive
	for _, a := range archives {
		var res unpack.Result
		var err error
		switch a.Type {
		case unpack.RarArchive:
			// Use 7z when preferred, or fall back to it when unrar
			// isn't available.
			use7z := opts.Prefer7zip
			if !use7z {
				unrarBin := opts.UnrarCommand
				if unrarBin == "" {
					unrarBin = "unrar"
				}
				if _, lookErr := exec.LookPath(unrarBin); lookErr != nil {
					use7z = true // unrar not found, fall back to 7z
					logf(log, job, slog.LevelInfo, "%s not found in PATH, falling back to 7z", unrarBin)
				}
			} else {
				logf(log, job, slog.LevelInfo, "Using 7z for RAR (prefer_7zip=true)")
			}
			if use7z {
				logf(log, job, slog.LevelInfo, "Running: 7z x %s", filepath.Base(a.MainFile))
				res, err = unpack.SevenZip(ctx, log, a, job.DownloadDir, opts)
			} else {
				unrarBin := opts.UnrarCommand
				if unrarBin == "" {
					unrarBin = "unrar"
				}
				logf(log, job, slog.LevelInfo, "Running: %s x %s", unrarBin, filepath.Base(a.MainFile))
				res, err = unpack.UnRAR(ctx, log, a, job.DownloadDir, opts)
			}
		case unpack.SevenZipArchive:
			logf(log, job, slog.LevelInfo, "Running: 7z x %s", filepath.Base(a.MainFile))
			res, err = unpack.SevenZip(ctx, log, a, job.DownloadDir, opts)
		case unpack.SplitArchive:
			logf(log, job, slog.LevelInfo, "Joining split files: %s (%d parts)", filepath.Base(a.MainFile), len(a.Parts))
			res, err = unpack.FileJoin(ctx, log, a, job.DownloadDir, opts)
		default:
			continue
		}
		if err != nil {
			job.UnpackError = true
			logf(log, job, slog.LevelWarn, "Error: extraction failed for %q (%s): %v", a.Name, archiveTypeName(a.Type), err)
			if firstErr == nil {
				firstErr = fmt.Errorf("unpack %q: %w", a.Name, err)
			}
			// Capture tool output even on error.
			if res.Output != "" {
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("[%s] %s (FAILED)", archiveTypeName(a.Type), a.Name))
				job.OutputLines = append(job.OutputLines,
					toolOutputLines(res.Output)...)
			}
			continue
		}
		if res.Err != nil {
			job.UnpackError = true
			logf(log, job, slog.LevelWarn, "Error: extraction result error for %q (%s): %v", a.Name, archiveTypeName(a.Type), res.Err)
			if firstErr == nil {
				firstErr = fmt.Errorf("unpack %q: %w", a.Name, res.Err)
			}
			// Capture tool output even on error.
			if res.Output != "" {
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("[%s] %s (FAILED)", archiveTypeName(a.Type), a.Name))
				job.OutputLines = append(job.OutputLines,
					toolOutputLines(res.Output)...)
			}
			continue
		}
		logf(log, job, slog.LevelInfo, "Extracted %s successfully", a.Name)
		// Capture tool output on success.
		if res.Output != "" {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[%s] %s", archiveTypeName(a.Type), a.Name))
			job.OutputLines = append(job.OutputLines,
				toolOutputLines(res.Output)...)
		}
		successfulArchives = append(successfulArchives, a)
	}

	// Delete source archive files after successful extraction.
	if u.Cleanup && len(successfulArchives) > 0 {
		var cleaned int
		for _, a := range successfulArchives {
			for _, part := range a.Parts {
				_ = os.Remove(part)
				cleaned++
			}
		}
		if cleaned > 0 {
			logf(log, job, slog.LevelInfo, "Cleaned up %d archive file(s)", cleaned)
		}
	}

	return firstErr
}

// DeobfuscateStage renames obfuscated files in place using the job's
// display name as the rename target. Scope matches the deobfuscate
// package — see its doc for the skipped Python behaviors.
type DeobfuscateStage struct {
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewDeobfuscateStage constructs a DeobfuscateStage.
func NewDeobfuscateStage() *DeobfuscateStage { return &DeobfuscateStage{} }

// Name returns the stage identifier.
func (*DeobfuscateStage) Name() string { return "deobfuscate" }

// Run invokes deobfuscate.Deobfuscate against job.DownloadDir.
func (d *DeobfuscateStage) Run(_ context.Context, job *Job) error {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "postproc/deobfuscate", "job", job.Queue.ID)

	logf(log, job, slog.LevelInfo, "Starting deobfuscation in %s (useful name: %s)", job.DownloadDir, job.Queue.Name)

	renames, err := deobfuscate.Deobfuscate(log, job.DownloadDir, job.Queue.Name, job.Sanitize)
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

// SortStage applies the first matching SorterRule to the job's files,
// moving them from job.DownloadDir into a derived path under DestRoot.
// When no rule matches, the stage is a no-op (files stay in DownloadDir;
// the caller can move them with a default rename).
type SortStage struct {
	// Rules is evaluated in order; first match wins.
	Rules []sorting.SorterRule

	// DestRoot is the absolute path under which matched rules place files.
	// The rule's SortString expands into a subpath beneath this.
	DestRoot string

	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewSortStage constructs a SortStage with the given rules and destination.
func NewSortStage(rules []sorting.SorterRule, destRoot string) *SortStage {
	return &SortStage{Rules: rules, DestRoot: destRoot}
}

// Name returns the stage identifier.
func (*SortStage) Name() string { return "sort" }

// Run picks the first matching rule and applies it.
func (s *SortStage) Run(ctx context.Context, job *Job) error {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "postproc/sort", "job", job.Queue.ID)

	// Skip sorting when earlier stages have failed — the files may be
	// incomplete or corrupt, and moving them to a "complete" directory
	// would be misleading. Matches Python's "if all_ok:" gate.
	if job.ParError || job.UnpackError {
		var reasons []string
		if job.ParError {
			reasons = append(reasons, "repair failed")
		}
		if job.UnpackError {
			reasons = append(reasons, "unpack failed")
		}
		logf(log, job, slog.LevelInfo, "Skipped: %s", strings.Join(reasons, ", "))
		return nil
	}

	logf(log, job, slog.LevelInfo, "Sorting: category=%q, name=%q, size=%d bytes, %d rule(s)",
		job.Queue.Category, job.Queue.Name, job.Queue.TotalBytes, len(s.Rules))

	res, err := sorting.Apply(ctx,
		log,
		job.DownloadDir,
		job.Queue.Category,
		job.Queue.Name,
		job.Queue.TotalBytes,
		s.Rules,
		s.DestRoot,
		job.Sanitize,
	)
	// Log sorting results.
	if res.MatchedRule != "" {
		logf(log, job, slog.LevelInfo, "Matched rule: %s", res.MatchedRule)
	} else {
		logf(log, job, slog.LevelInfo, "No sorting rule matched")
	}
	// Process partial results even on error — if some files were moved,
	// downstream stages must know where they are.
	if len(res.Moved) > 0 {
		for _, m := range res.Moved {
			logf(log, job, slog.LevelInfo, "%s → %s", filepath.Base(m.From), m.To)
		}
		origDir := job.DownloadDir
		job.FinalDir = filepath.Dir(res.Moved[0].To)
		// Point DownloadDir at the destination so downstream stages
		// (script, deobfuscate) operate on the actual files.
		job.DownloadDir = job.FinalDir
		logf(log, job, slog.LevelInfo, "Updated paths: downloadDir=%s, finalDir=%s", job.DownloadDir, job.FinalDir)
		// Only remove origDir if FinalDir is NOT inside it. If the
		// sorter moved files to a subdirectory of origDir, RemoveAll
		// would recursively delete the successfully moved files.
		cleanOrig, _ := filepath.Abs(origDir)
		cleanFinal, _ := filepath.Abs(job.FinalDir)
		if cleanOrig != cleanFinal && !strings.HasPrefix(cleanFinal, cleanOrig+string(filepath.Separator)) {
			logf(log, job, slog.LevelInfo, "Removing original directory: %s", origDir)
			_ = os.RemoveAll(origDir) // Clean up unmoved files/archives
		} else {
			logf(log, job, slog.LevelDebug, "Keeping original directory (final dir is inside it)")
		}
	}
	if err != nil {
		return fmt.Errorf("sort: %w", err)
	}
	return nil
}

// ScriptStage invokes the user's post-processing script (if any). A job
// with Script == "" or Script == "None" is skipped (matching Python).
type ScriptStage struct {
	// ScriptDir is the directory holding user scripts; the job's Script
	// field is resolved relative to it. May be absolute for portability.
	ScriptDir string

	// CompleteDir is the root complete directory surfaced to scripts as
	// SAB_COMPLETE_DIR and argv[1]. Distinct from Job.DownloadDir which
	// is the per-job incomplete working path.
	CompleteDir string

	// Version, APIKey, APIURL populate the corresponding SAB_* env vars.
	Version string
	APIKey  string
	APIURL  string

	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewScriptStage constructs a ScriptStage.
func NewScriptStage(scriptDir, completeDir, version, apiKey, apiURL string) *ScriptStage {
	return &ScriptStage{
		ScriptDir:   scriptDir,
		CompleteDir: completeDir,
		Version:     version,
		APIKey:      apiKey,
		APIURL:      apiURL,
	}
}

// Name returns the stage identifier.
func (*ScriptStage) Name() string { return "script" }

// Run builds a ScriptInput from the job and invokes RunScript. Returns nil
// when no script is configured or the script exits 0; wraps the RunScript
// error otherwise.
func (s *ScriptStage) Run(ctx context.Context, job *Job) error {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "postproc/script", "job", job.Queue.ID)

	name := job.Queue.Script
	if name == "" || name == "None" {
		logf(log, job, slog.LevelDebug, "No script configured")
		return nil
	}
	scriptPath := name
	if s.ScriptDir != "" && !filepath.IsAbs(name) {
		scriptPath = filepath.Join(s.ScriptDir, name)
	}

	status := 0
	if job.ParError || job.UnpackError || job.FailMsg != "" {
		status = 1
	}

	logf(log, job, slog.LevelInfo, "Running: %s", scriptPath)

	in := ScriptInput{
		FinalDir:    job.DownloadDir,
		CompleteDir: s.CompleteDir,
		NZBName:     job.Queue.Filename,
		JobName:     job.Queue.Name,
		Category:    job.Queue.Category,
		Status:      status,
		PPFlags:     job.Queue.PP,
		ScriptName:  name,
		NZOID:       job.Queue.ID,
		URL:         job.Queue.URL,
		Version:     s.Version,
		APIKey:      s.APIKey,
		APIURL:      s.APIURL,
		Bytes:       job.Queue.TotalBytes,
	}
	logf(log, job, slog.LevelInfo, "Script env: dir=%s, category=%s, status=%d, job=%s",
		in.FinalDir, in.Category, in.Status, in.JobName)

	res := RunScript(ctx, scriptPath, in)

	// Capture script output for the stage log.
	if res.LogBody != "" {
		job.OutputLines = append(job.OutputLines,
			toolOutputLines(res.LogBody)...)
	}
	logf(log, job, slog.LevelInfo, "Exit code: %d (%.1fs)", res.ExitCode, res.Duration.Seconds())

	if res.Err != nil {
		if errors.Is(res.Err, ErrNonZeroExit) {
			logf(log, job, slog.LevelWarn, "Error: script %q exited %d", name, res.ExitCode)
			return fmt.Errorf("script %q exited %d", name, res.ExitCode)
		}
		logf(log, job, slog.LevelWarn, "Error: script %q failed: %v", name, res.Err)
		return fmt.Errorf("script %q: %w", name, res.Err)
	}
	return nil
}

// FinalizeStage moves the completed job from job.DownloadDir to job.FinalDir.
// If FinalDir is not set, it defaults to placing the job folder (named after
// its ID) in the system's complete directory.
type FinalizeStage struct {
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
}

// NewFinalizeStage constructs a FinalizeStage.
func NewFinalizeStage() *FinalizeStage { return &FinalizeStage{} }

// Name returns the stage identifier.
func (*FinalizeStage) Name() string { return "finalize" }

// Run moves the directory content or the directory itself to its final location.
func (f *FinalizeStage) Run(ctx context.Context, job *Job) error {
	log := f.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "postproc/finalize", "job", job.Queue.ID)

	if job.ParError || job.UnpackError || job.FailMsg != "" {
		var reasons []string
		if job.ParError {
			reasons = append(reasons, "repair failed")
		}
		if job.UnpackError {
			reasons = append(reasons, "unpack failed")
		}
		if job.FailMsg != "" {
			reasons = append(reasons, job.FailMsg)
		}
		logf(log, job, slog.LevelInfo, "Skipped: files remain in download directory (%s)", strings.Join(reasons, "; "))
		return nil // Skip move if failed, so files stay in DownloadDir for retry
	}

	if job.FinalDir == "" {
		return fmt.Errorf("finalize: FinalDir not set")
	}

	if job.DownloadDir == job.FinalDir {
		logf(log, job, slog.LevelInfo, "Already at final location: %s", job.FinalDir)
		return nil // Already there (e.g. one-shot download directly to target)
	}

	logf(log, job, slog.LevelInfo, "Moving %s → %s", job.DownloadDir, job.FinalDir)

	// Create parent directory for final destination
	if err := os.MkdirAll(filepath.Dir(job.FinalDir), 0o750); err != nil {
		return fmt.Errorf("finalize: mkdir %s: %w", filepath.Dir(job.FinalDir), err)
	}

	// If the source directory exists, rename it to the target.
	// Fall back to file-by-file move on cross-device (EXDEV) or
	// not-empty (ENOTEMPTY/EEXIST) errors — the latter allows
	// merging files into an existing destination directory.
	if err := os.Rename(job.DownloadDir, job.FinalDir); err == nil {
		logf(log, job, slog.LevelInfo, "%s → %s (atomic rename)", job.DownloadDir, job.FinalDir)
		return nil
	} else if !fsutil.IsRenameMergeNeeded(err) {
		return fmt.Errorf("finalize: rename %s -> %s: %w", job.DownloadDir, job.FinalDir, err)
	} else {
		logf(log, job, slog.LevelInfo, "Atomic rename failed (%v), falling back to file-by-file move", err)
	}

	// Fallback: If os.Rename failed (e.g. cross-device), move file by file.
	entries, err := os.ReadDir(job.DownloadDir)
	if err != nil {
		return fmt.Errorf("finalize: readdir %s: %w", job.DownloadDir, err)
	}

	if err := os.MkdirAll(job.FinalDir, 0o750); err != nil {
		return fmt.Errorf("finalize: mkdir %s: %w", job.FinalDir, err)
	}

	for _, e := range entries {
		src := filepath.Join(job.DownloadDir, e.Name())
		dst := fsutil.JoinSafe(job.FinalDir, "", e.Name(), job.Sanitize)
		if err := moveRecursive(ctx, src, dst); err != nil {
			return fmt.Errorf("finalize: move %s -> %s: %w", src, dst, err)
		}
		logf(log, job, slog.LevelInfo, "%s → %s", filepath.Base(src), dst)
	}

	// Cleanup the empty source directory
	_ = os.RemoveAll(job.DownloadDir)
	logf(log, job, slog.LevelInfo, "Removed empty source directory: %s", job.DownloadDir)

	return nil
}

// moveRecursive handles moving files or directories, with cross-device support.
func moveRecursive(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	info, err := os.Lstat(src)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return fsutil.MoveFile(src, dst)
	}

	// It's a directory — preserve source permissions.
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if err := moveRecursive(ctx, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}

	return os.Remove(src)
}

// listNonPar2Files returns the full paths of all regular (non-directory)
// files in dir that are not .par2 files. These are passed as extra
// arguments to par2 repair so it can checksum-match files regardless of
// whether their names match the par2 set's file list.
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

// toolOutputLines splits raw tool output (stdout/stderr) into individual
// non-empty, trimmed lines suitable for the stage log.
func toolOutputLines(raw string) []string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimRight(l, "\r \t")
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// archiveTypeName returns a human-readable label for an ArchiveType constant.
func archiveTypeName(t unpack.ArchiveType) string {
	switch t {
	case unpack.RarArchive:
		return "unrar"
	case unpack.SevenZipArchive:
		return "7zip"
	case unpack.SplitArchive:
		return "filejoin"
	default:
		return "unpack"
	}
}
