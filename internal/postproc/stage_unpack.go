package postproc

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// UnpackStage extracts downloaded archives during post-processing.
type UnpackStage struct {
	// toggle provides the thread-safe SetEnabled/enabled flag.
	toggle
	// mu protects BaseOpts, Permissions, PasswordFile, EnableFileJoin, and EnableRecursive.
	// Can be updated via Set* methods from the API goroutine while a job runs.
	mu sync.RWMutex
	// BaseOpts holds config-driven extraction options (tool paths, flags).
	// The job's password is merged at runtime.
	BaseOpts unpack.Options
	// Permissions is an octal string (e.g. "755") applied recursively
	// after successful extraction. Dirs get the full mode, files get
	// execute bits stripped. Empty disables chmod.
	Permissions string

	// cleanup is set atomically so SetCleanup can be called from any goroutine.
	cleanup atomic.Bool
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
func NewUnpackStage() *UnpackStage {
	return &UnpackStage{}
}

// NewUnpackStageWith constructs an UnpackStage with the given base options.
func NewUnpackStageWith(opts unpack.Options, cleanup bool) *UnpackStage {
	s := &UnpackStage{BaseOpts: opts}
	s.cleanup.Store(cleanup)
	return s
}

// SetCleanup enables or disables archive file deletion at runtime without
// requiring a server restart. Thread-safe; may be called from any goroutine.
func (u *UnpackStage) SetCleanup(enabled bool) { u.cleanup.Store(enabled) }

// SetBaseOpts updates the extraction base options. Thread-safe.
func (u *UnpackStage) SetBaseOpts(opts unpack.Options) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts = opts
}

// SetPasswordFile updates the global password file path. Thread-safe.
func (u *UnpackStage) SetPasswordFile(v string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.PasswordFile = v
}

// SetEnableFileJoin enables or disables split file joining. Thread-safe.
func (u *UnpackStage) SetEnableFileJoin(v bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.EnableFileJoin = v
}

// SetEnableRecursive enables or disables recursive unpacking. Thread-safe.
func (u *UnpackStage) SetEnableRecursive(v bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.EnableRecursive = v
}

// SetOverwriteFiles enables or disables overwriting existing files on extraction.
// Thread-safe; takes effect for the next job.
func (u *UnpackStage) SetOverwriteFiles(v bool) {
	u.mu.Lock()
	u.BaseOpts.OverwriteFiles = v
	u.mu.Unlock()
}

// SetFlatUnpack enables or disables flat extraction (ignore archive directories).
// Thread-safe; takes effect for the next job.
func (u *UnpackStage) SetFlatUnpack(v bool) {
	u.mu.Lock()
	u.BaseOpts.OneFolder = v
	u.mu.Unlock()
}

// SetPermissions updates the octal permission string applied after extraction.
// Thread-safe; takes effect for the next job.
func (u *UnpackStage) SetPermissions(v string) {
	u.mu.Lock()
	u.Permissions = v
	u.mu.Unlock()
}

// SetUnrarCommand updates the unrar binary path at runtime. Thread-safe.
func (u *UnpackStage) SetUnrarCommand(v string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts.UnrarCommand = v
}

// SetSevenZipCommand updates the 7z binary path at runtime. Thread-safe.
func (u *UnpackStage) SetSevenZipCommand(v string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts.SevenZipCommand = v
}

// SetUseGoRAR updates built-in RAR extractor toggle at runtime. Thread-safe.
func (u *UnpackStage) SetUseGoRAR(v bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts.UseGoRAR = v
}

// SetGoRarFallback updates pure-Go fallback settings at runtime. Thread-safe.
func (u *UnpackStage) SetGoRarFallback(v bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts.GoRarFallback = v
}

// SetUseGo7z updates built-in 7z extractor toggle at runtime. Thread-safe.
func (u *UnpackStage) SetUseGo7z(v bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts.UseGo7z = v
}

// SetGo7zFallback updates pure-Go 7z fallback settings at runtime. Thread-safe.
func (u *UnpackStage) SetGo7zFallback(v bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts.Go7zFallback = v
}

// SetNiceAndIonice updates nice and ionice settings at runtime. Thread-safe.
func (u *UnpackStage) SetNiceAndIonice(nice, ionice string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts.CmdCfg.Nice = nice
	u.BaseOpts.CmdCfg.Ionice = ionice
}

// SetExtraUnrarParams updates unrar extra parameters at runtime. Thread-safe.
func (u *UnpackStage) SetExtraUnrarParams(args []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts.ExtraArgs = args
}

// SetIgnoreUnrarDates updates ignore archive dates setting at runtime. Thread-safe.
func (u *UnpackStage) SetIgnoreUnrarDates(v bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts.IgnoreUnrarDates = v
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

	if !u.enabled() {
		logf(ctx, log, job, slog.LevelInfo, "Unpack stage disabled — skipping")
		return nil
	}

	// Skip extraction when repair has already failed — the archives are
	// corrupt and unpacking would produce garbage. Matches Python's
	// safe_postproc gate: "if all_ok: ... unpacker()".
	if job.ParError {
		logf(ctx, log, job, slog.LevelInfo, "Skipped: repair failed, archives may be corrupt")
		return nil
	}

	// Snapshot mutable fields under RLock so a concurrent SetOverwriteFiles /
	// SetFlatUnpack / SetPermissions / SetPasswordFile / SetEnableFileJoin / SetEnableRecursive call doesn't race.
	u.mu.RLock()
	opts := u.BaseOpts
	permissions := u.Permissions
	passwordFile := u.PasswordFile
	enableFileJoin := u.EnableFileJoin
	enableRecursive := u.EnableRecursive
	u.mu.RUnlock()

	opts = u.prepareOptions(ctx, log, job, opts, passwordFile)

	// Track which archives have been processed across all passes to
	// avoid re-extracting the same archive in a subsequent pass.
	processed := make(map[string]bool)
	var allSuccessful []unpack.Archive
	var firstErr error

	// DirectUnpack: if any RAR sets were already extracted during download,
	// mark their main archive files as processed so Scan → extract skips them.
	// The archive parts are still recorded in allSuccessful for cleanup.
	u.handleDirectUnpack(ctx, log, job, processed, &allSuccessful)

	maxDepth := maxUnpackDepth
	if !enableRecursive {
		maxDepth = 1
	}
	for depth := range maxDepth {
		logf(ctx, log, job, slog.LevelInfo, "Scanning for archives in %s (pass %d/%d)", job.DownloadDir, depth+1, maxUnpackDepth)

		archives, err := unpack.Scan(job.DownloadDir)
		if err != nil {
			job.UnpackError = true
			return fmt.Errorf("unpack: scan: %w", err)
		}

		// Filter out already-processed archives.
		pending := u.filterPending(archives, processed)

		if len(pending) == 0 {
			if depth == 0 {
				logf(ctx, log, job, slog.LevelInfo, "No archives found")
			} else {
				logf(ctx, log, job, slog.LevelInfo, "No new archives found (pass %d)", depth+1)
			}
			break
		}

		logf(ctx, log, job, slog.LevelInfo, "Found %d archive(s) (pass %d)", len(pending), depth+1)

		// Sort pending archives so file joins (SplitArchive) run first,
		// then RAR extraction, then 7z. This matches SABnzbd's unpacker()
		// order: file_join → rar_unpack → unseven. Without this, joined
		// output can't be extracted in the same pass.
		slices.SortStableFunc(pending, func(a, b unpack.Archive) int {
			return cmp.Compare(archiveTypePriority(a.Type), archiveTypePriority(b.Type))
		})

		for _, a := range pending {
			logf(ctx, log, job, slog.LevelInfo, "  %s: %s (%d parts)", archiveTypeName(a.Type), a.Name, len(a.Parts))
		}

		extractedAny := u.extractPendingArchives(ctx, log, job, pending, processed, opts, enableFileJoin, &firstErr, &allSuccessful)
		// If nothing was extracted this pass, no point re-scanning.
		if !extractedAny {
			break
		}
	}

	// Delete source archive files after successful extraction.
	u.cleanupArchives(ctx, log, job, allSuccessful)

	// Apply permissions recursively after extraction.
	u.applyPermissions(ctx, log, job, permissions, allSuccessful)

	return firstErr
}

func (u *UnpackStage) extractPendingArchives(
	ctx context.Context,
	log *slog.Logger,
	job *Job,
	pending []unpack.Archive,
	processed map[string]bool,
	opts unpack.Options,
	enableFileJoin bool,
	firstErr *error, //nolint:gocritic // ptrToRefParam: pointer required to accumulate first error across calls
	allSuccessful *[]unpack.Archive,
) bool {
	extractedAny := false
	for _, a := range pending {
		processed[a.MainFile] = true

		res, err := u.extractArchive(ctx, log, job, a, opts, enableFileJoin)
		if failErr := cmp.Or(err, res.Err); failErr != nil {
			recordUnpackFailure(ctx, log, job, a, res, failErr, firstErr)
			continue
		}
		logf(ctx, log, job, slog.LevelInfo, "Extracted %s successfully", a.Name)
		// Record command line in stage log for successful extractions too.
		if res.CommandLine != "" {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[%s] %s", cmp.Or(res.Engine, archiveTypeName(a.Type)), a.Name),
				"Command: "+res.CommandLine)
		}

		// Post-extraction containment check: verify all extracted files
		// are inside the output directory. A malicious archive could
		// contain paths like "../../../etc/crontab" that escape outDir.
		if cErr := fsutil.CheckContainment(job.DownloadDir); cErr != nil {
			job.UnpackError = true
			logf(ctx, log, job, slog.LevelWarn, "Error: containment violation after extracting %q: %v", a.Name, cErr)
			if *firstErr == nil {
				*firstErr = fmt.Errorf("unpack %q: containment check: %w", a.Name, cErr)
			}
			continue
		}

		// Capture tool output on success.
		if res.Output != "" {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[%s] %s", cmp.Or(res.Engine, archiveTypeName(a.Type)), a.Name))
			job.OutputLines = append(job.OutputLines,
				toolOutputLines(res.Output)...)
		}
		*allSuccessful = append(*allSuccessful, a)
		extractedAny = true
	}
	return extractedAny
}

// prepareOptions sets up passwords list, callbacks, and other extraction options.
func (u *UnpackStage) prepareOptions(ctx context.Context, log *slog.Logger, job *Job, opts unpack.Options, passwordFile string) unpack.Options {
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
	if passwordFile != "" {
		filePws, err := cmdutil.LoadPasswordFile(passwordFile)
		if err != nil {
			logf(ctx, log, job, slog.LevelWarn, "password file: %v", err)
		} else {
			opts.Passwords = append(opts.Passwords, filePws...)
			if len(filePws) > 30 {
				logf(ctx, log, job, slog.LevelWarn, "password file contains %d entries (>30); extraction may be slow", len(filePws))
			}
		}
	}
	opts.Password = "" //nolint:staticcheck // SA1019: intentionally zeroed to force Passwords-list iteration
	if len(opts.Passwords) > 0 {
		logf(ctx, log, job, slog.LevelInfo, "Will try %d password(s) for encrypted archives", len(opts.Passwords))
	}
	return opts
}

// handleDirectUnpack marks DirectUnpack archive files as processed and appends them to allSuccessful.
func (u *UnpackStage) handleDirectUnpack(ctx context.Context, log *slog.Logger, job *Job, processed map[string]bool, allSuccessful *[]unpack.Archive) {
	if len(job.DirectUnpackSets) > 0 {
		for setname, result := range job.DirectUnpackSets {
			logf(ctx, log, job, slog.LevelInfo, "Set %q already extracted by DirectUnpack (%d files, %d parts)",
				setname, len(result.ExtractedFiles), len(result.RarParts))
			for _, part := range result.RarParts {
				processed[part] = true
				// Also mark by basename so Scan's file-based lookup matches.
				processed[filepath.Base(part)] = true
			}
			*allSuccessful = append(*allSuccessful, unpack.Archive{
				Type:  unpack.RarArchive,
				Parts: result.RarParts,
			})
		}
	}
}

// filterPending returns a list of archives that haven't been processed yet.
func (u *UnpackStage) filterPending(archives []unpack.Archive, processed map[string]bool) []unpack.Archive {
	var pending []unpack.Archive
	for _, a := range archives {
		if !processed[a.MainFile] {
			pending = append(pending, a)
		}
	}
	return pending
}

// extractArchive executes extraction based on the archive type.
func (u *UnpackStage) extractArchive(ctx context.Context, log *slog.Logger, job *Job, a unpack.Archive, opts unpack.Options, enableFileJoin bool) (unpack.Result, error) {
	switch a.Type {
	case unpack.RarArchive:
		return extractRARArchive(ctx, log, job, a, opts)
	case unpack.SevenZipArchive:
		return extractSevenZipArchive(ctx, log, job, a, opts)
	case unpack.SplitArchive:
		if !enableFileJoin {
			logf(ctx, log, job, slog.LevelInfo, "Skipping file join (disabled): %s", filepath.Base(a.MainFile))
			return unpack.Result{}, nil
		}
		logf(ctx, log, job, slog.LevelInfo, "Joining split files: %s (%d parts)", filepath.Base(a.MainFile), len(a.Parts))
		return unpack.FileJoin(ctx, log, a, job.DownloadDir, opts)
	default:
		return unpack.Result{}, nil
	}
}

// cleanupArchives deletes source archive files after successful extraction.
func (u *UnpackStage) cleanupArchives(ctx context.Context, log *slog.Logger, job *Job, allSuccessful []unpack.Archive) {
	if u.cleanup.Load() && len(allSuccessful) > 0 {
		var cleaned int
		for _, a := range allSuccessful {
			for _, part := range a.Parts {
				_ = os.Remove(part)
				cleaned++
			}
		}
		if cleaned > 0 {
			logf(ctx, log, job, slog.LevelInfo, "Cleaned up %d archive file(s)", cleaned)
		}
	}
}

// applyPermissions recursively applies target permissions to the extracted files.
func (u *UnpackStage) applyPermissions(ctx context.Context, log *slog.Logger, job *Job, permissions string, allSuccessful []unpack.Archive) {
	if permissions != "" && len(allSuccessful) > 0 {
		applied, permErr := applyPermissions(job.DownloadDir, permissions)
		if permErr != nil {
			logf(ctx, log, job, slog.LevelWarn, "permissions: %v", permErr)
		} else if applied > 0 {
			logf(ctx, log, job, slog.LevelInfo, "Applied permissions (%s) to %d file(s)/dir(s)", permissions, applied)
		}
	}
}

// extractRARArchive dispatches a single RAR archive to either GoUnRAR
// (pure-Go) or the external unrar binary, following the same priority order
// as SABnzbd:
//
//  1. use_go_rar → GoUnRAR; fall back to unrar subprocess on failure
//  2. unrar available → unrar subprocess
//  3. unrar not in PATH → GoUnRAR
func extractRARArchive(ctx context.Context, log *slog.Logger, job *Job, a unpack.Archive, opts unpack.Options) (unpack.Result, error) {
	useGoRAR := opts.UseGoRAR
	if !useGoRAR {
		unrarBin := opts.UnrarCommand
		if unrarBin == "" {
			unrarBin = "unrar"
		}
		if _, lookErr := exec.LookPath(unrarBin); lookErr != nil {
			useGoRAR = true
			logf(ctx, log, job, slog.LevelInfo, "%s not found in PATH, falling back to go_unrar", unrarBin)
			if job.OnOutput != nil {
				job.OnOutput("go_unrar", fmt.Sprintf("%s not found in PATH, falling back to go_unrar", unrarBin))
			}
		}
	}

	if !useGoRAR {
		unrarOpts := opts
		unrarOpts.OnLine = func(line string) {
			if job.OnOutput != nil {
				job.OnOutput("unrar", line)
			}
		}
		unrarOpts.OnCommand = func(cmdLine string) {
			logf(ctx, log, job, slog.LevelInfo, "Running: %s", cmdLine)
			if job.OnOutput != nil {
				job.OnOutput("unrar", fmt.Sprintf("Running command: %s", cmdLine))
			}
		}
		if job.OnOutput != nil {
			job.OnOutput("unrar", fmt.Sprintf("Unpacking: %s (using external unrar)", a.Name))
		}
		res, err := unpack.UnRARWithPasswords(ctx, log, a, job.DownloadDir, unrarOpts)
		if err == nil && res.Err == nil {
			if job.OnOutput != nil {
				job.OnOutput("unrar", fmt.Sprintf("Unpacking complete: %s (using external unrar)", a.Name))
			}
		}
		return res, err
	}

	logf(ctx, log, job, slog.LevelInfo, "Using go_unrar for RAR (pure-Go)")
	if job.OnOutput != nil {
		job.OnOutput("go_unrar", fmt.Sprintf("Using go_unrar for RAR (pure-Go): %s", a.Name))
	}
	goOpts := opts
	goOpts.OnLine = func(line string) {
		if job.OnOutput != nil {
			job.OnOutput("go_unrar", line)
		}
	}
	res, err := unpack.GoUnRARWithPasswords(ctx, log, a, job.DownloadDir, goOpts)
	if err == nil && res.Err == nil {
		if job.OnOutput != nil {
			job.OnOutput("go_unrar", fmt.Sprintf("go_unrar: unpacking complete: %s", a.Name))
		}
	}

	// Fallback: if Go-native extraction failed and the external unrar binary
	// is available, retry with it. rarengine only supports RAR3/RAR5; other
	// formats and some encrypted archives require the external binary.
	// Gated on GoRarFallback so users can disable the retry.
	if goErr := cmp.Or(err, res.Err); goErr != nil && opts.GoRarFallback {
		unrarBin := opts.UnrarCommand
		if unrarBin == "" {
			unrarBin = "unrar"
		}
		if _, lookErr := exec.LookPath(unrarBin); lookErr == nil {
			logf(ctx, log, job, slog.LevelWarn,
				"go_unrar failed (%v), retrying with external %s", goErr, unrarBin)
			if job.OnOutput != nil {
				job.OnOutput("go_unrar",
					fmt.Sprintf("Go-native extraction failed: %v — retrying with %s", goErr, unrarBin))
			}
			unrarOpts := opts
			unrarOpts.OnLine = func(line string) {
				if job.OnOutput != nil {
					job.OnOutput("unrar", line)
				}
			}
			unrarOpts.OnCommand = func(cmdLine string) {
				logf(ctx, log, job, slog.LevelInfo, "Running: %s", cmdLine)
				if job.OnOutput != nil {
					job.OnOutput("unrar", fmt.Sprintf("Running command: %s", cmdLine))
				}
			}
			res, err := unpack.UnRARWithPasswords(ctx, log, a, job.DownloadDir, unrarOpts)
			if err == nil && res.Err == nil {
				if job.OnOutput != nil {
					job.OnOutput("unrar", fmt.Sprintf("Unpacking complete: %s (using external unrar)", a.Name))
				}
			}
			return res, err
		}
	}
	return res, err
}

// extractSevenZipArchive dispatches a single 7-Zip archive to either
// GoSevenZip (pure-Go) or the external 7z binary, following the same
// priority order as SABnzbd:
//
//  1. use_go_7z → GoSevenZip; fall back to 7z subprocess on failure
//  2. 7z available → 7z subprocess
//  3. 7z not in PATH → GoSevenZip
func extractSevenZipArchive(ctx context.Context, log *slog.Logger, job *Job, a unpack.Archive, opts unpack.Options) (unpack.Result, error) {
	useGo7z := opts.UseGo7z
	if !useGo7z {
		szBin := opts.SevenZipCommand
		if szBin == "" {
			szBin = "7zz"
		}
		if _, lookErr := exec.LookPath(szBin); lookErr != nil {
			if _, lookErr2 := exec.LookPath("7z"); lookErr2 != nil {
				useGo7z = true
				logf(ctx, log, job, slog.LevelInfo, "7z binary not found in PATH, falling back to go_7z")
				if job.OnOutput != nil {
					job.OnOutput("go_7z", "7z binary not found in PATH, falling back to go_7z")
				}
			}
		}
	}

	if !useGo7z {
		szOpts := opts
		szOpts.OnLine = func(line string) {
			if job.OnOutput != nil {
				job.OnOutput("7z", line)
			}
		}
		szOpts.OnCommand = func(cmdLine string) {
			logf(ctx, log, job, slog.LevelInfo, "Running: %s", cmdLine)
			if job.OnOutput != nil {
				job.OnOutput("7z", fmt.Sprintf("Running command: %s", cmdLine))
			}
		}
		if job.OnOutput != nil {
			job.OnOutput("7z", fmt.Sprintf("Unpacking: %s (using external 7z)", a.Name))
		}
		res, err := unpack.SevenZipWithPasswords(ctx, log, a, job.DownloadDir, szOpts)
		if err == nil && res.Err == nil {
			if job.OnOutput != nil {
				job.OnOutput("7z", fmt.Sprintf("Unpacking complete: %s (using external 7z)", a.Name))
			}
		}
		return res, err
	}

	logf(ctx, log, job, slog.LevelInfo, "Using go_7z for 7-Zip (pure-Go)")
	if job.OnOutput != nil {
		job.OnOutput("go_7z", fmt.Sprintf("Using go_7z for 7-Zip (pure-Go): %s", a.Name))
	}
	goOpts := opts
	goOpts.OnLine = func(line string) {
		if job.OnOutput != nil {
			job.OnOutput("go_7z", line)
		}
	}
	res, err := unpack.GoSevenZipWithPasswords(ctx, log, a, job.DownloadDir, goOpts)
	if err == nil && res.Err == nil {
		if job.OnOutput != nil {
			job.OnOutput("go_7z", fmt.Sprintf("go_7z: unpacking complete: %s", a.Name))
		}
	}

	// Fallback: if Go-native extraction failed and the external 7z binary
	// is available, retry with it. Gated on Go7zFallback so users can
	// disable the retry.
	if goErr := cmp.Or(err, res.Err); goErr != nil && opts.Go7zFallback {
		szBin := find7zBin(opts.SevenZipCommand)
		if szBin != "" {
			logf(ctx, log, job, slog.LevelWarn,
				"go_7z failed (%v), retrying with external %s", goErr, szBin)
			if job.OnOutput != nil {
				job.OnOutput("go_7z",
					fmt.Sprintf("Go-native extraction failed: %v — retrying with %s", goErr, szBin))
			}
			szOpts := opts
			szOpts.SevenZipCommand = szBin
			szOpts.OnLine = func(line string) {
				if job.OnOutput != nil {
					job.OnOutput("7z", line)
				}
			}
			szOpts.OnCommand = func(cmdLine string) {
				logf(ctx, log, job, slog.LevelInfo, "Running: %s", cmdLine)
				if job.OnOutput != nil {
					job.OnOutput("7z", fmt.Sprintf("Running command: %s", cmdLine))
				}
			}
			res, err := unpack.SevenZipWithPasswords(ctx, log, a, job.DownloadDir, szOpts)
			if err == nil && res.Err == nil {
				if job.OnOutput != nil {
					job.OnOutput("7z", fmt.Sprintf("Unpacking complete: %s (using external 7z)", a.Name))
				}
			}
			return res, err
		}
	}
	return res, err
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
	fileMode := dirMode &^ 0o111 // strip execute bits for regular files

	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, fmt.Errorf("open root %s: %w", dir, err)
	}
	defer root.Close() //nolint:errcheck // read-only close

	var count int
	walkErr := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			//nolint:nilerr // ignore stat error to continue walking
			return nil // skip entries we can't stat
		}
		if path == "." {
			return nil
		}
		var target os.FileMode
		switch {
		case d.IsDir():
			target = dirMode
		case d.Type().IsRegular():
			target = fileMode
		default:
			return nil // skip symlinks, devices, etc.
		}
		if chErr := root.Chmod(path, target); chErr == nil {
			count++
		}
		return nil
	})
	if walkErr != nil {
		return count, fmt.Errorf("walk %s: %w", dir, walkErr)
	}
	return count, nil
}

// recordUnpackFailure handles extraction failures by setting job error
// state, logging the failure, capturing the first error, and appending
// command/output lines for diagnostics.
func recordUnpackFailure(ctx context.Context, log *slog.Logger, job *Job, a unpack.Archive, res unpack.Result, failErr error, firstErr *error) { //nolint:gocritic // ptrToRefParam: pointer required to set caller's first-error sentinel
	job.UnpackError = true
	engine := cmp.Or(res.Engine, archiveTypeName(a.Type))
	logf(ctx, log, job, slog.LevelWarn, "Error: extraction failed for %q (%s): %v", a.Name, engine, failErr)
	if *firstErr == nil {
		*firstErr = fmt.Errorf("unpack %q: %w", a.Name, failErr)
	}
	job.OutputLines = append(job.OutputLines,
		fmt.Sprintf("[%s] %s (FAILED)", engine, a.Name))
	if res.CommandLine != "" {
		job.OutputLines = append(job.OutputLines, "Command: "+res.CommandLine)
	}
	if res.Output != "" {
		job.OutputLines = append(job.OutputLines,
			toolOutputLines(res.Output)...)
	}
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

// find7zBin returns the path to an available 7z binary, checking the
// configured name first, then "7zz", then "7z". Returns "" if none found.
func find7zBin(configured string) string {
	if configured != "" {
		if _, err := exec.LookPath(configured); err == nil {
			return configured
		}
	}
	if _, err := exec.LookPath("7zz"); err == nil {
		return "7zz"
	}
	if _, err := exec.LookPath("7z"); err == nil {
		return "7z"
	}
	return ""
}

// CleanupStage removes temporary admin data from the download directory after
// post-processing completes. On successful jobs the __ADMIN__ directory
// (containing verified sets, crash recovery data) is removed. On failed
// jobs the data is preserved for debugging/retry.
//
// This corresponds to Python's nzo.purge_data() — spec §6.2 step 15.
