package postproc

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
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
	// mu protects BaseOpts, Permissions, PasswordFile, EnableFileJoin, EnableTar, and EnableRecursive.
	// These are replaced atomically via Apply from the API goroutine while a job runs.
	mu sync.RWMutex
	// BaseOpts holds config-driven extraction options (tool paths, flags).
	// The job's password is merged at runtime.
	BaseOpts unpack.Options
	// Permissions is an octal string (e.g. "755") applied recursively
	// after successful extraction. Dirs get the full mode, files get
	// execute bits stripped. Empty disables chmod.
	Permissions string

	// cleanup is set atomically (by Apply) so it can be updated from any goroutine.
	cleanup atomic.Bool
	// PasswordFile is the path to a text file with one password per line.
	// These are appended after per-job passwords during extraction.
	PasswordFile string
	// EnableFileJoin enables split file joining (.001/.002).
	// When false, SplitArchive types are skipped. Default true.
	EnableFileJoin bool
	// EnableTar enables plain .tar extraction. When false, TarArchive
	// types are skipped entirely (matching the EnableFileJoin pattern).
	// Default true.
	EnableTar bool
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

// UnpackConfig is the full set of runtime-mutable UnpackStage settings, applied
// atomically by Apply. It uses the stage's own option types (unpack.Options),
// keeping internal/postproc free of any dependency on internal/config: the
// caller (internal/app) owns the config→UnpackConfig translation.
//
// It does not include the SetEnabled toggle (pipeline on/off, a separate
// concern) — that stays on SetEnabled.
type UnpackConfig struct {
	// Base is the full extraction option set, including construction-only
	// fields (HasProblem, Sandbox.Enabled) which the caller carries forward.
	Base            unpack.Options
	Permissions     string
	PasswordFile    string
	EnableFileJoin  bool
	EnableTar       bool
	EnableRecursive bool
	Cleanup         bool
}

// Apply atomically replaces all runtime-mutable state from c under a single
// lock. It supersedes the former per-field Set* methods so a reload is one
// atomic swap rather than many independently-locked writes a running job could
// interleave with. Thread-safe; takes effect for the next job.
func (u *UnpackStage) Apply(c UnpackConfig) {
	u.cleanup.Store(c.Cleanup) // atomic, matching the former SetCleanup
	u.mu.Lock()
	defer u.mu.Unlock()
	u.BaseOpts = c.Base
	u.Permissions = c.Permissions
	u.PasswordFile = c.PasswordFile
	u.EnableFileJoin = c.EnableFileJoin
	u.EnableTar = c.EnableTar
	u.EnableRecursive = c.EnableRecursive
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
	log = log.With("component", "unpack", "job", job.JobID())

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

	// Snapshot mutable fields under RLock so a concurrent Apply doesn't race.
	u.mu.RLock()
	opts := u.BaseOpts
	permissions := u.Permissions
	passwordFile := u.PasswordFile
	enableFileJoin := u.EnableFileJoin
	enableTar := u.EnableTar
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
			logf(ctx, log, job, slog.LevelInfo, "  %s: %s (%d parts)", archiveTypeName(a.Type), a.MainFile, len(a.Parts))
		}

		extractedAny := u.extractPendingArchives(ctx, log, job, pending, processed, opts, enableFileJoin, enableTar, &firstErr, &allSuccessful)
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
	enableTar bool,
	firstErr *error, //nolint:gocritic // ptrToRefParam: pointer required to accumulate first error across calls
	allSuccessful *[]unpack.Archive,
) bool {
	extractedAny := false
	for _, a := range pending {
		processed[a.MainFile] = true

		res, err := u.extractArchive(ctx, log, job, a, opts, enableFileJoin, enableTar)
		failErr := err
		if failErr == nil {
			failErr = res.Err
		}
		if failErr != nil {
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
			cleanupContainmentViolation(job.DownloadDir, res.ExtractedFiles, log)
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
		markOwned(job, res.ExtractedFiles)
		*allSuccessful = append(*allSuccessful, a)
		extractedAny = true
	}
	return extractedAny
}

// prepareOptions sets up passwords list, callbacks, and other extraction options.
func (u *UnpackStage) prepareOptions(ctx context.Context, log *slog.Logger, job *Job, opts unpack.Options, passwordFile string) unpack.Options {
	if opts.Sandbox.TargetDir == "" {
		opts.Sandbox.TargetDir = job.DownloadDir
	}
	opts.OnLine = func(line string) {
		if job.OnOutput != nil {
			job.OnOutput("unpack", line)
		}
	}
	// Build the password list: per-job password comes first (highest priority),
	// followed by any passwords from the config password_file.
	if job.Password != "" {
		opts.Passwords = append([]string{job.Password}, opts.Passwords...)
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
			markOwned(job, result.ExtractedFiles)
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
func (u *UnpackStage) extractArchive(ctx context.Context, log *slog.Logger, job *Job, a unpack.Archive, opts unpack.Options, enableFileJoin, enableTar bool) (unpack.Result, error) {
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
	case unpack.TarArchive:
		if !enableTar {
			logf(ctx, log, job, slog.LevelInfo, "Skipping tar archive (disabled): %s", filepath.Base(a.MainFile))
			return unpack.Result{}, nil
		}
		return extractTarArchive(ctx, log, job, a, opts)
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
				if err := fsutil.Remove(part); err == nil {
					line := "Deleted archive file: " + filepath.Base(part)
					job.OutputLines = append(job.OutputLines, "[unpack] "+line)
					if job.OnOutput != nil {
						job.OnOutput("unpack", line)
					}
					cleaned++
				}
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

type archiveEngineDriver struct {
	toolName    string
	goToolName  string
	formatName  string
	useGo       bool
	fallback    bool
	findBin     func(opts unpack.Options) (string, error)
	runExternal func(ctx context.Context, log *slog.Logger, a unpack.Archive, outDir string, opts unpack.Options) (unpack.Result, error)
	runGo       func(ctx context.Context, log *slog.Logger, a unpack.Archive, outDir string, opts unpack.Options) (unpack.Result, error)
	onExternal  func(opts *unpack.Options, bin string)
}

func extractWithDriver(ctx context.Context, log *slog.Logger, job *Job, a unpack.Archive, opts unpack.Options, d archiveEngineDriver) (unpack.Result, error) {
	useGo := d.useGo
	var extBin string
	if !useGo {
		var binErr error
		extBin, binErr = d.findBin(opts)
		if binErr != nil {
			useGo = true
			msg := fmt.Sprintf("%s not found in PATH, falling back to %s", cmp.Or(extBin, d.toolName+" binary"), d.goToolName)
			logf(ctx, log, job, slog.LevelInfo, "%s", msg)
			if job.OnOutput != nil {
				job.OnOutput(d.goToolName, msg)
			}
		}
	}

	if !useGo {
		extOpts := opts
		if d.onExternal != nil {
			d.onExternal(&extOpts, extBin)
		}
		extOpts.OnLine = func(line string) {
			if job.OnOutput != nil {
				job.OnOutput(d.toolName, line)
			}
		}
		extOpts.OnCommand = func(cmdLine string) {
			logf(ctx, log, job, slog.LevelInfo, "Running: %s", cmdLine)
			if job.OnOutput != nil {
				job.OnOutput(d.toolName, fmt.Sprintf("Running command: %s", cmdLine))
			}
		}
		if job.OnOutput != nil {
			job.OnOutput(d.toolName, fmt.Sprintf("Unpacking: %s (using external %s)", a.Name, d.toolName))
		}
		res, err := d.runExternal(ctx, log, a, job.DownloadDir, extOpts)
		if err == nil && res.Err == nil {
			if job.OnOutput != nil {
				job.OnOutput(d.toolName, fmt.Sprintf("Unpacking complete: %s (using external %s)", a.Name, d.toolName))
			}
		}
		return res, err
	}

	logf(ctx, log, job, slog.LevelInfo, "Using %s for %s (pure-Go)", d.goToolName, d.formatName)
	if job.OnOutput != nil {
		job.OnOutput(d.goToolName, fmt.Sprintf("Using %s for %s (pure-Go): %s", d.goToolName, d.formatName, a.Name))
	}
	goOpts := opts
	goOpts.OnLine = func(line string) {
		if job.OnOutput != nil {
			job.OnOutput(d.goToolName, line)
		}
	}
	res, err := d.runGo(ctx, log, a, job.DownloadDir, goOpts)
	if err == nil && res.Err == nil {
		if job.OnOutput != nil {
			job.OnOutput(d.goToolName, fmt.Sprintf("%s: unpacking complete: %s", d.goToolName, a.Name))
		}
	}

	goErr := err
	if goErr == nil {
		goErr = res.Err
	}
	if goErr != nil && d.fallback {
		extBin, binErr := d.findBin(opts)
		if binErr == nil {
			logf(ctx, log, job, slog.LevelWarn,
				"%s failed (%v), retrying with external %s", d.goToolName, goErr, extBin)
			if job.OnOutput != nil {
				job.OnOutput(d.goToolName,
					fmt.Sprintf("Go-native extraction failed: %v — retrying with %s", goErr, extBin))
			}
			extOpts := opts
			if d.onExternal != nil {
				d.onExternal(&extOpts, extBin)
			}
			extOpts.OnLine = func(line string) {
				if job.OnOutput != nil {
					job.OnOutput(d.toolName, line)
				}
			}
			extOpts.OnCommand = func(cmdLine string) {
				logf(ctx, log, job, slog.LevelInfo, "Running: %s", cmdLine)
				if job.OnOutput != nil {
					job.OnOutput(d.toolName, fmt.Sprintf("Running command: %s", cmdLine))
				}
			}
			res, err := d.runExternal(ctx, log, a, job.DownloadDir, extOpts)
			if err == nil && res.Err == nil {
				if job.OnOutput != nil {
					job.OnOutput(d.toolName, fmt.Sprintf("Unpacking complete: %s (using external %s)", a.Name, d.toolName))
				}
			}
			return res, err
		}
	}
	return res, err
}

// extractRARArchive dispatches a single RAR archive to either GoUnRAR
// (pure-Go) or the external unrar binary, following the same priority order
// as SABnzbd:
//
//  1. use_go_rar → GoUnRAR; fall back to unrar subprocess on failure
//  2. unrar available → unrar subprocess
//  3. unrar not in PATH → GoUnRAR
func extractRARArchive(ctx context.Context, log *slog.Logger, job *Job, a unpack.Archive, opts unpack.Options) (unpack.Result, error) {
	return extractWithDriver(ctx, log, job, a, opts, archiveEngineDriver{
		toolName:    "unrar",
		goToolName:  "go_unrar",
		formatName:  "RAR",
		useGo:       opts.UseGoRAR,
		fallback:    opts.GoRarFallback,
		findBin:     unpack.UnrarBin,
		runExternal: unpack.UnRARWithPasswords,
		runGo:       unpack.GoUnRARWithPasswords,
	})
}

// extractSevenZipArchive dispatches a single 7-Zip archive to either
// GoSevenZip (pure-Go) or the external 7z binary, following the same
// priority order as SABnzbd:
//
//  1. use_go_7z → GoSevenZip; fall back to 7z subprocess on failure
//  2. 7z available → 7z subprocess
//  3. 7z not in PATH → GoSevenZip
func extractSevenZipArchive(ctx context.Context, log *slog.Logger, job *Job, a unpack.Archive, opts unpack.Options) (unpack.Result, error) {
	return extractWithDriver(ctx, log, job, a, opts, archiveEngineDriver{
		toolName:    "7z",
		goToolName:  "go_7z",
		formatName:  "7-Zip",
		useGo:       opts.UseGo7z,
		fallback:    opts.Go7zFallback,
		findBin:     unpack.SevenZipBin,
		runExternal: unpack.SevenZipWithPasswords,
		runGo:       unpack.GoSevenZipWithPasswords,
		onExternal: func(o *unpack.Options, bin string) {
			o.SevenZipCommand = bin
		},
	})
}

// extractTarArchive dispatches a single plain .tar archive to GoTar.
// Unlike RAR/7z there is no engine-selection option or external-tool
// fallback: Go's stdlib archive/tar is the only extraction path, so this
// is a direct call rather than going through extractWithDriver.
func extractTarArchive(ctx context.Context, log *slog.Logger, job *Job, a unpack.Archive, opts unpack.Options) (unpack.Result, error) {
	logf(ctx, log, job, slog.LevelInfo, "Using go_tar for tar (pure-Go): %s", a.Name)
	if job.OnOutput != nil {
		job.OnOutput("go_tar", fmt.Sprintf("Unpacking: %s (using go_tar)", a.Name))
	}
	tarOpts := opts
	tarOpts.OnLine = func(line string) {
		if job.OnOutput != nil {
			job.OnOutput("go_tar", line)
		}
	}
	res, err := unpack.GoTar(ctx, log, a, job.DownloadDir, tarOpts)
	if err == nil && res.Err == nil {
		if job.OnOutput != nil {
			job.OnOutput("go_tar", fmt.Sprintf("go_tar: unpacking complete: %s", a.Name))
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

func archiveTypeName(t unpack.ArchiveType) string {
	switch t {
	case unpack.RarArchive:
		return "rar"
	case unpack.SevenZipArchive:
		return "7zip"
	case unpack.SplitArchive:
		return "filejoin"
	case unpack.TarArchive:
		return "tar"
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
	case unpack.TarArchive:
		return 3 // then tar
	default:
		return 4
	}
}

// cleanupContainmentViolation removes the entries an extraction created when it
// fails the containment check. It deletes ONLY paths that lie inside outDir,
// and it unlinks them without following symlinks. It must never delete the
// resolved target of an out-of-bounds symlink: a malicious archive can plant a
// symlink (e.g. link -> /home/user) inside outDir, and following it to
// os.RemoveAll the target would let the archive destroy arbitrary pre-existing
// files. Unlinking the symlink itself neutralizes the escape while leaving the
// victim untouched.
func cleanupContainmentViolation(outDir string, extractedFiles []string, log *slog.Logger) {
	for _, f := range extractedFiles {
		absPath := f
		if !filepath.IsAbs(f) {
			absPath = filepath.Join(outDir, f)
		}
		// Guard lexically against the extracted path itself (not its symlink
		// target) so an escaping symlink is unlinked here rather than followed.
		if !fsutil.PathWithin(outDir, absPath) {
			if log != nil {
				log.Warn("containment: refusing to remove extracted path outside output dir", "file", absPath)
			}
			continue
		}
		// fsutil.Remove unlinks a symlink without following it, so an out-of-bounds
		// symlink target is left intact. Extracted entries are regular files
		// (directories are never recorded by the snapshot diff), so Remove
		// suffices and avoids recursively deleting through a bad path.
		if rmErr := fsutil.Remove(absPath); rmErr == nil && log != nil {
			log.Warn("containment: removed extracted file after containment check failure", "file", absPath)
		}
	}
}

// CleanupStage removes temporary admin data from the download directory after
// post-processing completes. On successful jobs the __ADMIN__ directory
// (containing verified sets, crash recovery data) is removed. On failed
// jobs the data is preserved for debugging/retry.
//
// This corresponds to Python's nzo.purge_data() — spec §6.2 step 15.
