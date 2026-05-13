package postproc

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/unpack"
)

type UnpackStage struct {
	// BaseOpts holds config-driven extraction options (tool paths, flags).
	// The job's password is merged at runtime.
	BaseOpts unpack.Options
	// Cleanup deletes source archive files after successful extraction.
	Cleanup bool
	// Permissions is an octal string (e.g. "755") applied recursively
	// after successful extraction. Dirs get the full mode, files get
	// execute bits stripped. Empty disables chmod.
	Permissions string
	// PasswordFile is the path to a text file with one password per line.
	// These are appended after per-job passwords during extraction.
	PasswordFile string
	// EnableFileJoin enables split file joining (.001/.002).
	// When false, SplitArchive types are skipped. Default true.
	EnableFileJoin bool
	// EnableRecursive enables recursive unpacking (re-scan after each
	// pass, up to maxUnpackDepth). When false, only one pass runs.
	EnableRecursive bool
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

// maxUnpackDepth limits recursive unpacking to prevent infinite loops
// from self-extracting or circular archives. Matches SABnzbd's limit.
const maxUnpackDepth = 3

// Run scans job.DownloadDir, routes each archive to the right unpack
// function, and captures any failures on job.UnpackError. Implements
// recursive unpacking: after each extraction pass, re-scans for new
// archives (up to maxUnpackDepth passes) to handle nested archives.
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

	// Merge config-level options with per-job password.
	opts := u.BaseOpts
	opts.OnLine = func(line string) {
		if job.OnOutput != nil {
			job.OnOutput("unpack", line)
		}
	}
	// Build the password list: per-job password comes first (highest priority),
	// followed by any passwords from the config password_file.
	if job.Queue.Password != "" {
		opts.Passwords = append([]string{job.Queue.Password}, opts.Passwords...)
	}
	if u.PasswordFile != "" {
		filePws, err := cmdutil.LoadPasswordFile(u.PasswordFile)
		if err != nil {
			logf(log, job, slog.LevelWarn, "password file: %v", err)
		} else {
			opts.Passwords = append(opts.Passwords, filePws...)
			if len(filePws) > 30 {
				logf(log, job, slog.LevelWarn, "password file contains %d entries (>30); extraction may be slow", len(filePws))
			}
		}
	}
	opts.Password = "" //nolint:staticcheck // SA1019: intentionally zeroed to force Passwords-list iteration
	if len(opts.Passwords) > 0 {
		logf(log, job, slog.LevelInfo, "Will try %d password(s) for encrypted archives", len(opts.Passwords))
	}

	// Track which archives have been processed across all passes to
	// avoid re-extracting the same archive in a subsequent pass.
	processed := make(map[string]bool)
	var allSuccessful []unpack.Archive
	var firstErr error

	maxDepth := maxUnpackDepth
	if !u.EnableRecursive {
		maxDepth = 1
	}
	for depth := range maxDepth {
		logf(log, job, slog.LevelInfo, "Scanning for archives in %s (pass %d/%d)", job.DownloadDir, depth+1, maxUnpackDepth)

		archives, err := unpack.Scan(job.DownloadDir)
		if err != nil {
			job.UnpackError = true
			return fmt.Errorf("unpack: scan: %w", err)
		}

		// Filter out already-processed archives.
		var pending []unpack.Archive
		for _, a := range archives {
			if !processed[a.MainFile] {
				pending = append(pending, a)
			}
		}

		if len(pending) == 0 {
			if depth == 0 {
				logf(log, job, slog.LevelInfo, "No archives found")
			} else {
				logf(log, job, slog.LevelInfo, "No new archives found (pass %d)", depth+1)
			}
			break
		}

		logf(log, job, slog.LevelInfo, "Found %d archive(s) (pass %d)", len(pending), depth+1)

		// I5: Sort pending archives so file joins (SplitArchive) run first,
		// then RAR extraction, then 7z. This matches SABnzbd's unpacker()
		// order: file_join → rar_unpack → unseven. Without this, joined
		// output can't be extracted in the same pass.
		slices.SortStableFunc(pending, func(a, b unpack.Archive) int {
			return cmp.Compare(archiveTypePriority(a.Type), archiveTypePriority(b.Type))
		})

		for _, a := range pending {
			logf(log, job, slog.LevelInfo, "  %s: %s (%d parts)", archiveTypeName(a.Type), a.Name, len(a.Parts))
		}

		extractedAny := false
		for _, a := range pending {
			processed[a.MainFile] = true

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
					szOpts := opts
					szOpts.OnLine = func(line string) {
						if job.OnOutput != nil {
							job.OnOutput("7z", line)
						}
					}
					szOpts.OnCommand = func(cmdLine string) {
						logf(log, job, slog.LevelInfo, "Running: %s", cmdLine)
					}
					res, err = unpack.SevenZipWithPasswords(ctx, log, a, job.DownloadDir, szOpts)
				} else {
					unrarOpts := opts
					unrarOpts.OnLine = func(line string) {
						if job.OnOutput != nil {
							job.OnOutput("unrar", line)
						}
					}
					unrarOpts.OnCommand = func(cmdLine string) {
						logf(log, job, slog.LevelInfo, "Running: %s", cmdLine)
					}
					res, err = unpack.UnRARWithPasswords(ctx, log, a, job.DownloadDir, unrarOpts)
				}
			case unpack.SevenZipArchive:
				szOpts := opts
				szOpts.OnLine = func(line string) {
					if job.OnOutput != nil {
						job.OnOutput("7z", line)
					}
				}
				szOpts.OnCommand = func(cmdLine string) {
					logf(log, job, slog.LevelInfo, "Running: %s", cmdLine)
				}
				res, err = unpack.SevenZipWithPasswords(ctx, log, a, job.DownloadDir, szOpts)
			case unpack.SplitArchive:
				if !u.EnableFileJoin {
					logf(log, job, slog.LevelInfo, "Skipping file join (disabled): %s", filepath.Base(a.MainFile))
					continue
				}
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
				// Capture command line and tool output even on error.
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("[%s] %s (FAILED)", archiveTypeName(a.Type), a.Name))
				if res.CommandLine != "" {
					job.OutputLines = append(job.OutputLines, "Command: "+res.CommandLine)
				}
				if res.Output != "" {
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
				// Capture command line and tool output even on error.
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("[%s] %s (FAILED)", archiveTypeName(a.Type), a.Name))
				if res.CommandLine != "" {
					job.OutputLines = append(job.OutputLines, "Command: "+res.CommandLine)
				}
				if res.Output != "" {
					job.OutputLines = append(job.OutputLines,
						toolOutputLines(res.Output)...)
				}
				continue
			}
			logf(log, job, slog.LevelInfo, "Extracted %s successfully", a.Name)
			// Record command line in stage log for successful extractions too.
			if res.CommandLine != "" {
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("[%s] %s", archiveTypeName(a.Type), a.Name),
					"Command: "+res.CommandLine)
			}

			// Post-extraction containment check: verify all extracted files
			// are inside the output directory. A malicious archive could
			// contain paths like "../../../etc/crontab" that escape outDir.
			if cErr := fsutil.CheckContainment(job.DownloadDir); cErr != nil {
				job.UnpackError = true
				logf(log, job, slog.LevelWarn, "Error: containment violation after extracting %q: %v", a.Name, cErr)
				if firstErr == nil {
					firstErr = fmt.Errorf("unpack %q: containment check: %w", a.Name, cErr)
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
			allSuccessful = append(allSuccessful, a)
			extractedAny = true
		}

		// If nothing was extracted this pass, no point re-scanning.
		if !extractedAny {
			break
		}
	}

	// Delete source archive files after successful extraction.
	if u.Cleanup && len(allSuccessful) > 0 {
		var cleaned int
		for _, a := range allSuccessful {
			for _, part := range a.Parts {
				_ = os.Remove(part)
				cleaned++
			}
		}
		if cleaned > 0 {
			logf(log, job, slog.LevelInfo, "Cleaned up %d archive file(s)", cleaned)
		}
	}

	// Apply permissions recursively after extraction.
	if u.Permissions != "" && len(allSuccessful) > 0 {
		applied, permErr := applyPermissions(job.DownloadDir, u.Permissions)
		if permErr != nil {
			logf(log, job, slog.LevelWarn, "permissions: %v", permErr)
		} else if applied > 0 {
			logf(log, job, slog.LevelInfo, "Applied permissions (%s) to %d file(s)/dir(s)", u.Permissions, applied)
		}
	}

	return firstErr
}

// applyPermissions walks dir recursively and applies the given octal
// permission string. Directories receive the full mode; regular files
// have execute bits stripped (e.g. "755" → dirs=0755, files=0644).
// Returns the number of entries changed and any error encountered during
// permission parsing (walk errors are logged but don't stop traversal).
func applyPermissions(dir, permStr string) (int, error) {
	mode, err := strconv.ParseUint(permStr, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid permission string %q: %w", permStr, err)
	}
	dirMode := os.FileMode(mode)
	fileMode := dirMode &^ 0111 // strip execute bits for regular files

	var count int
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip entries we can't stat
		}
		var target os.FileMode
		if d.IsDir() {
			target = dirMode
		} else if d.Type().IsRegular() {
			target = fileMode
		} else {
			return nil // skip symlinks, devices, etc.
		}
		if chErr := os.Chmod(path, target); chErr == nil {
			count++
		}
		return nil
	})
	if walkErr != nil {
		return count, fmt.Errorf("walk %s: %w", dir, walkErr)
	}
	return count, nil
}

// RecoverPar2NamesStage renames obfuscated files using par2 metadata
// (16K-MD5 matching). This mirrors SABnzbd's recover_par2_names() which
// runs after unpacking but before heuristic deobfuscation. It is
// unconditional — it runs even when deobfuscate_filenames is disabled.
//
// Par2 files contain MD5 hashes of the first 16384 bytes of each file.
// This stage matches every file in the download directory against those
// hashes and renames matches to the par2-recorded names.
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

// archiveTypePriority returns a sort key that ensures archives are processed
// in SABnzbd's order: file joins first (so joined output can be extracted
// in the same pass), then RAR, then 7z.
func archiveTypePriority(t unpack.ArchiveType) int {
	switch t {
	case unpack.SplitArchive:
		return 0 // joins first
	case unpack.RarArchive:
		return 1 // then RAR
	case unpack.SevenZipArchive:
		return 2 // then 7z
	default:
		return 3
	}
}

// CleanupStage removes temporary admin data from the download directory after
// post-processing completes. On successful jobs the __ADMIN__ directory
// (containing verified sets, crash recovery data) is removed. On failed
// jobs the data is preserved for debugging/retry.
//
// This corresponds to Python's nzo.purge_data() — spec §6.2 step 15.
