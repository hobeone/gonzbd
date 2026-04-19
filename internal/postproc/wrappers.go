package postproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hobeone/sabnzbd-go/internal/deobfuscate"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/par2"
	"github.com/hobeone/sabnzbd-go/internal/sorting"
	"github.com/hobeone/sabnzbd-go/internal/unpack"
)

// RepairStage runs par2 verify+repair against every par2 set it finds in
// the job's DownloadDir. A set with status RepairNotPossible or an exec
// failure sets job.ParError; the pipeline continues (unpack may still
// succeed on an intact archive).
type RepairStage struct{}

// NewRepairStage constructs a RepairStage.
func NewRepairStage() *RepairStage { return &RepairStage{} }

// Name returns the stage identifier.
func (*RepairStage) Name() string { return "repair" }

// Run finds par2 sets in job.DownloadDir and repairs each. No-op when the
// job has no par2 files.
func (*RepairStage) Run(ctx context.Context, job *Job) error {
	sets, err := par2.FindPar2Files(job.DownloadDir)
	if err != nil {
		job.ParError = true
		return fmt.Errorf("repair: find par2 sets: %w", err)
	}

	// Scan for temporary files written by the assembler.
	tmpFiles, _ := filepath.Glob(filepath.Join(job.DownloadDir, "*.tmp"))

	var firstErr error
	if len(sets) > 0 {
		for _, set := range sets {
			main := set.MainFile
			if main == "" && len(set.ExtraFiles) > 0 {
				main = set.ExtraFiles[0]
			}
			if main == "" {
				continue
			}
			res, err := par2.Repair(ctx, main, tmpFiles...)
			if err != nil {
				job.ParError = true
				if firstErr == nil {
					firstErr = fmt.Errorf("repair %q: %w", set.Name, err)
				}
				continue
			}
			if !res.Success {
				job.ParError = true
				if firstErr == nil {
					firstErr = fmt.Errorf("repair %q: unsuccessful (exit=%d)", set.Name, res.ExitCode)
				}
			}
		}
	}

	// Fallback: Rename any remaining *.tmp files using NZB metadata/subject.
	// This handles jobs without PAR2 files and ensures we have correct
	// filenames for the Unpack stage.
	remainingTmp, _ := filepath.Glob(filepath.Join(job.DownloadDir, "*.tmp"))
	for _, tmpPath := range remainingTmp {
		base := filepath.Base(tmpPath)
		var fileIdx int
		if _, err := fmt.Sscanf(base, "%04d.tmp", &fileIdx); err != nil {
			continue
		}
		if fileIdx < 0 || fileIdx >= len(job.Queue.Files) {
			continue
		}

		cleanName := nzb.ExtractFilenameFromSubject(job.Queue.Files[fileIdx].Subject)
		destPath := filepath.Join(job.DownloadDir, cleanName)

		// Don't overwrite existing files
		if _, err := os.Stat(destPath); err == nil {
			continue
		}

		if err := os.Rename(tmpPath, destPath); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("fallback rename %q -> %q: %w", base, cleanName, err)
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
// stage moves them. The caller is expected to clean archive files after
// the pipeline completes if they want Python's delete-originals behavior;
// UnpackStage itself never deletes.
type UnpackStage struct{}

// NewUnpackStage constructs an UnpackStage.
func NewUnpackStage() *UnpackStage { return &UnpackStage{} }

// Name returns the stage identifier.
func (*UnpackStage) Name() string { return "unpack" }

// Run scans job.DownloadDir, routes each archive to the right unpack
// function, and captures any failures on job.UnpackError.
func (*UnpackStage) Run(ctx context.Context, job *Job) error {
	archives, err := unpack.Scan(job.DownloadDir)
	if err != nil {
		job.UnpackError = true
		return fmt.Errorf("unpack: scan: %w", err)
	}
	if len(archives) == 0 {
		return nil
	}
	opts := unpack.Options{
		Password: job.Queue.Password,
	}
	var firstErr error
	for _, a := range archives {
		var res unpack.Result
		var err error
		switch a.Type {
		case unpack.RarArchive:
			res, err = unpack.UnRAR(ctx, a, job.DownloadDir, opts)
		case unpack.SevenZipArchive:
			res, err = unpack.SevenZip(ctx, a, job.DownloadDir, opts)
		case unpack.SplitArchive:
			res, err = unpack.FileJoin(ctx, a, job.DownloadDir, opts)
		default:
			continue
		}
		if err != nil {
			job.UnpackError = true
			if firstErr == nil {
				firstErr = fmt.Errorf("unpack %q: %w", a.Name, err)
			}
			continue
		}
		if res.Err != nil {
			job.UnpackError = true
			if firstErr == nil {
				firstErr = fmt.Errorf("unpack %q: %w", a.Name, res.Err)
			}
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
	if _, err := deobfuscate.Deobfuscate(job.DownloadDir, job.Queue.Name); err != nil {
		return fmt.Errorf("deobfuscate: %w", err)
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
	_, err := sorting.Apply(ctx,
		job.DownloadDir,
		job.Queue.Category,
		job.Queue.Name,
		job.Queue.TotalBytes,
		s.Rules,
		s.DestRoot,
	)
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
	// Use sorting.MoveFile for cross-device support.
	// Since we want to move the WHOLE directory, we'll try os.Rename first.
	if err := os.Rename(job.DownloadDir, job.FinalDir); err == nil {
		return nil
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
		dst := filepath.Join(job.FinalDir, e.Name())
		if err := moveRecursive(ctx, src, dst); err != nil {
			return fmt.Errorf("finalize: move %s -> %s: %w", src, dst, err)
		}
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
		return moveFile(src, dst)
	}

	// It's a directory.
	if err := os.MkdirAll(dst, 0o750); err != nil {
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

// moveFile matches the one in sorting package but internal for now to avoid circular deps
// or excessive exports if we don't want to export moveFile from sorting.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Simplified cross-device: copy + delete
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

