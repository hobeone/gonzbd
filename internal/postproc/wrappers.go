package postproc

import (
	"context"
	"errors"
	"fmt"
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
	sets, err := par2.FindPar2Files(job.DownloadDir)
	if err != nil {
		job.ParError = true
		return fmt.Errorf("repair: find par2 sets: %w", err)
	}

	var firstErr error
	repairSucceeded := true
	if len(sets) > 0 {
		job.OutputLines = append(job.OutputLines,
			fmt.Sprintf("Found %d par2 set(s)", len(sets)))

		// Collect all non-par2 files in the download directory to pass as
		// extra arguments. This lets par2 checksum-match files even when
		// their names don't match the par2 set's expectations (e.g.
		// obfuscated or renamed files).
		dataFiles, scanErr := listNonPar2Files(job.DownloadDir)
		if scanErr != nil {
			job.ParError = true
			return fmt.Errorf("repair: scan data files: %w", scanErr)
		}

		for _, set := range sets {
			main := set.MainFile
			if main == "" && len(set.ExtraFiles) > 0 {
				main = set.ExtraFiles[0]
			}
			if main == "" {
				continue
			}
			par2Bin := s.Par2Opts.Command
			if par2Bin == "" {
				par2Bin = "par2"
			}
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("Running: %s r %s (+%d data files)", par2Bin, filepath.Base(main), len(dataFiles)))
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
				if firstErr == nil {
					firstErr = fmt.Errorf("repair %q: %w", set.Name, err)
				}
				continue
			}
			if !res.Success {
				job.ParError = true
				repairSucceeded = false
				if firstErr == nil {
					firstErr = fmt.Errorf("repair %q: unsuccessful (exit=%d)", set.Name, res.ExitCode)
				}
			}
		}
	} else {
		job.OutputLines = append(job.OutputLines, "No par2 files found")
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
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("Cleaned up %d par2 file(s)", cleaned))
			}
		}
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
	// Skip extraction when repair has already failed — the archives are
	// corrupt and unpacking would produce garbage. Matches Python's
	// safe_postproc gate: "if all_ok: ... unpacker()".
	if job.ParError {
		job.OutputLines = append(job.OutputLines,
			"Skipped: repair failed, archives may be corrupt")
		return nil
	}

	archives, err := unpack.Scan(job.DownloadDir)
	if err != nil {
		job.UnpackError = true
		return fmt.Errorf("unpack: scan: %w", err)
	}
	if len(archives) == 0 {
		job.OutputLines = append(job.OutputLines, "No archives found")
		return nil
	}
	job.OutputLines = append(job.OutputLines,
		fmt.Sprintf("Found %d archive(s)", len(archives)))
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
				}
			}
			if use7z {
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("Running: 7z x %s", filepath.Base(a.MainFile)))
				res, err = unpack.SevenZip(ctx, a, job.DownloadDir, opts)
			} else {
				unrarBin := opts.UnrarCommand
				if unrarBin == "" {
					unrarBin = "unrar"
				}
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("Running: %s x %s", unrarBin, filepath.Base(a.MainFile)))
				res, err = unpack.UnRAR(ctx, a, job.DownloadDir, opts)
			}
		case unpack.SevenZipArchive:
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("Running: 7z x %s", filepath.Base(a.MainFile)))
			res, err = unpack.SevenZip(ctx, a, job.DownloadDir, opts)
		case unpack.SplitArchive:
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("Joining split files: %s (%d parts)", filepath.Base(a.MainFile), len(a.Parts)))
			res, err = unpack.FileJoin(ctx, a, job.DownloadDir, opts)
		default:
			continue
		}
		if err != nil {
			job.UnpackError = true
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
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("Cleaned up %d archive file(s)", cleaned))
		}
	}

	return firstErr
}

// DeobfuscateStage renames obfuscated files in place using the job's
// display name as the rename target. Scope matches the deobfuscate
// package — see its doc for the skipped Python behaviors.
type DeobfuscateStage struct{}

// NewDeobfuscateStage constructs a DeobfuscateStage.
func NewDeobfuscateStage() *DeobfuscateStage { return &DeobfuscateStage{} }

// Name returns the stage identifier.
func (*DeobfuscateStage) Name() string { return "deobfuscate" }

// Run invokes deobfuscate.Deobfuscate against job.DownloadDir.
func (*DeobfuscateStage) Run(_ context.Context, job *Job) error {
	renames, err := deobfuscate.Deobfuscate(job.DownloadDir, job.Queue.Name, job.Sanitize)
	if len(renames) == 0 {
		job.OutputLines = append(job.OutputLines, "No files needed deobfuscation")
	} else {
		job.OutputLines = append(job.OutputLines,
			fmt.Sprintf("Deobfuscated %d file(s)", len(renames)))
		for _, r := range renames {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("%s → %s", filepath.Base(r.From), filepath.Base(r.To)))
		}
	}

	// Subtitle alignment: rename .srt files to match the dominant video.
	subRenames, subErr := deobfuscate.Subtitles(job.DownloadDir)
	if len(subRenames) > 0 {
		job.OutputLines = append(job.OutputLines,
			fmt.Sprintf("Renamed %d subtitle file(s)", len(subRenames)))
		for _, r := range subRenames {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("%s → %s", filepath.Base(r.From), filepath.Base(r.To)))
		}
	}
	// Prefer to report the primary deobfuscation error if both failed.
	if err != nil {
		return fmt.Errorf("deobfuscate: %w", err)
	}
	if subErr != nil {
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
}

// NewSortStage constructs a SortStage with the given rules and destination.
func NewSortStage(rules []sorting.SorterRule, destRoot string) *SortStage {
	return &SortStage{Rules: rules, DestRoot: destRoot}
}

// Name returns the stage identifier.
func (*SortStage) Name() string { return "sort" }

// Run picks the first matching rule and applies it.
func (s *SortStage) Run(ctx context.Context, job *Job) error {
	// Skip sorting when earlier stages have failed — the files may be
	// incomplete or corrupt, and moving them to a "complete" directory
	// would be misleading. Matches Python's "if all_ok:" gate.
	if job.ParError || job.UnpackError {
		job.OutputLines = append(job.OutputLines,
			"Skipped: earlier stage failed")
		return nil
	}

	res, err := sorting.Apply(ctx,
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
		job.OutputLines = append(job.OutputLines,
			fmt.Sprintf("Matched rule: %s", res.MatchedRule))
	} else {
		job.OutputLines = append(job.OutputLines, "No sorting rule matched")
	}
	// Process partial results even on error — if some files were moved,
	// downstream stages must know where they are.
	if len(res.Moved) > 0 {
		for _, m := range res.Moved {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("%s → %s", filepath.Base(m.From), m.To))
		}
		origDir := job.DownloadDir
		job.FinalDir = filepath.Dir(res.Moved[0].To)
		// Point DownloadDir at the destination so downstream stages
		// (script, deobfuscate) operate on the actual files.
		job.DownloadDir = job.FinalDir
		// Only remove origDir if FinalDir is NOT inside it. If the
		// sorter moved files to a subdirectory of origDir, RemoveAll
		// would recursively delete the successfully moved files.
		cleanOrig, _ := filepath.Abs(origDir)
		cleanFinal, _ := filepath.Abs(job.FinalDir)
		if cleanOrig != cleanFinal && !strings.HasPrefix(cleanFinal, cleanOrig+string(filepath.Separator)) {
			_ = os.RemoveAll(origDir) // Clean up unmoved files/archives
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
	name := job.Queue.Script
	if name == "" || name == "None" {
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

	job.OutputLines = append(job.OutputLines,
		fmt.Sprintf("Running: %s", scriptPath))

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

	res := RunScript(ctx, scriptPath, in)

	// Capture script output for the stage log.
	if res.LogBody != "" {
		job.OutputLines = append(job.OutputLines,
			toolOutputLines(res.LogBody)...)
	}
	job.OutputLines = append(job.OutputLines,
		fmt.Sprintf("Exit code: %d (%.1fs)", res.ExitCode, res.Duration.Seconds()))

	if res.Err != nil {
		if errors.Is(res.Err, ErrNonZeroExit) {
			return fmt.Errorf("script %q exited %d", name, res.ExitCode)
		}
		return fmt.Errorf("script %q: %w", name, res.Err)
	}
	return nil
}

// FinalizeStage moves the completed job from job.DownloadDir to job.FinalDir.
// If FinalDir is not set, it defaults to placing the job folder (named after
// its ID) in the system's complete directory.
type FinalizeStage struct{}

// NewFinalizeStage constructs a FinalizeStage.
func NewFinalizeStage() *FinalizeStage { return &FinalizeStage{} }

// Name returns the stage identifier.
func (*FinalizeStage) Name() string { return "finalize" }

// Run moves the directory content or the directory itself to its final location.
func (*FinalizeStage) Run(ctx context.Context, job *Job) error {
	if job.ParError || job.UnpackError || job.FailMsg != "" {
		job.OutputLines = append(job.OutputLines,
			"Skipped: files remain in download directory for retry")
		return nil // Skip move if failed, so files stay in DownloadDir for retry
	}

	if job.FinalDir == "" {
		return fmt.Errorf("finalize: FinalDir not set")
	}

	if job.DownloadDir == job.FinalDir {
		return nil // Already there (e.g. one-shot download directly to target)
	}

	// Create parent directory for final destination
	if err := os.MkdirAll(filepath.Dir(job.FinalDir), 0o750); err != nil {
		return fmt.Errorf("finalize: mkdir %s: %w", filepath.Dir(job.FinalDir), err)
	}

	// If the source directory exists, rename it to the target.
	// Fall back to file-by-file move on cross-device (EXDEV) or
	// not-empty (ENOTEMPTY/EEXIST) errors — the latter allows
	// merging files into an existing destination directory.
	if err := os.Rename(job.DownloadDir, job.FinalDir); err == nil {
		job.OutputLines = append(job.OutputLines,
			fmt.Sprintf("%s → %s", job.DownloadDir, job.FinalDir))
		return nil
	} else if !fsutil.IsRenameMergeNeeded(err) {
		return fmt.Errorf("finalize: rename %s -> %s: %w", job.DownloadDir, job.FinalDir, err)
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
		job.OutputLines = append(job.OutputLines,
			fmt.Sprintf("%s → %s", filepath.Base(src), dst))
	}

	// Cleanup the empty source directory
	_ = os.RemoveAll(job.DownloadDir)

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
