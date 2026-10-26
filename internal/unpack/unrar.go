package unpack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

// Options controls how an extraction tool is invoked.
type Options struct {
	// Passwords is a prioritized list of passwords to try. When an
	// extraction fails with a "wrong password" error, the next password
	// in the list is tried. This implements SABnzbd's get_all_passwords()
	// behavior. Empty means no password. Callers that already know there's
	// exactly one password can still set a single-element list.
	//
	// UnRAR, SevenZip, GoUnRAR, and GoSevenZip take the password for one
	// attempt as an explicit function parameter. The *WithPasswords
	// wrappers (via withPasswords) iterate Passwords, calling the
	// underlying extractor once per candidate. GoUnRAR (through
	// goUnRAREngine) also reads it: given an empty password parameter, it
	// hands the whole of Passwords to rarengine (go_unrar.go's
	// r.SetPasswords(opts.Passwords)).
	Passwords []string
	// OneFolder extracts all files flat into outDir (no path preservation),
	// and a directory member creates nothing. For unrar this selects the
	// 'e' command; for 7z this selects 'e' instead of 'x'.
	OneFolder bool
	// KeepOriginals controls whether the caller should delete the source Parts
	// after a successful extraction.  The extractors do not read it and do
	// not delete the source Parts; that deletion is the caller's
	// responsibility. (They do remove their own staging directories and
	// temp files.)
	KeepOriginals bool

	// UnrarCommand overrides the unrar binary path. Empty defaults to "unrar".
	UnrarCommand string
	// SevenZipCommand overrides the 7z binary path. Empty uses auto-detection.
	SevenZipCommand string
	// OverwriteFiles controls what extraction does with a name that is
	// already taken, by an earlier member inside the staging directory every
	// extractor writes into, or by a file in outDir when the stage is
	// published. true overwrites it, in every engine (-o+ for unrar, -aoa
	// for 7z) and at publish. false (the default) never overwrites, and what
	// it does instead depends on OneFolder:
	//   - without OneFolder, the member is skipped and the existing file
	//     kept: unrar is passed -o-, 7z -aos, the in-process engines
	//     (GoUnRAR, GoSevenZip, GoTar) skip the write, and the publish
	//     skips the name;
	//   - with OneFolder, the member is renamed rather than skipped: unrar is
	//     passed -o- -or ("name(1).ext"), 7z -aou ("name_1.ext"), and the
	//     in-process engines and the publish use a uniquePath "_N" name. The
	//     one exception is unrar with HasProblem set, which is passed -o-
	//     alone and skips a member whose flattened name an earlier member
	//     took.
	OverwriteFiles bool
	// IgnoreUnrarDates discards in-archive modification timestamps and uses extraction time.
	// Adds -tsm- to unrar arguments (matches SABnzbd's behavior).
	IgnoreUnrarDates bool
	// DecodeWorkers is the configured rar_decode_workers value for the
	// pure-Go RAR path: <= 0 means auto, 1 serial, larger values decode a
	// compressed member's blocks on that many goroutines (see
	// DecodeWorkers). The external unrar is not governed by this option.
	DecodeWorkers int
	// UseGoRAR uses the pure-Go rarengine library for RAR extraction instead
	// of shelling out to unrar: RAR5, and the stored members of RAR 1.5-4.x.
	// No external binary required. Default true.
	UseGoRAR bool
	// GoRarFallback allows a failed pure-Go RAR extraction to retry with
	// the external unrar binary. Only relevant when UseGoRAR is true.
	// Default true.
	GoRarFallback bool
	// UseGo7z uses the pure-Go bodgit/sevenzip library for 7z extraction
	// instead of shelling out to 7zz/7z. No external binary required.
	// Default true.
	UseGo7z bool
	// Go7zFallback allows a failed pure-Go 7-Zip extraction to retry with
	// the external 7z binary. Only relevant when UseGo7z is true.
	// Default true.
	Go7zFallback bool
	// CmdCfg controls nice/ionice process priority wrapping for the
	// extraction subprocess. When non-empty, the command is prepended
	// with nice and/or ionice. Matches SABnzbd's cfg.nice/cfg.ionice.
	CmdCfg cmdutil.CmdConfig
	// Sandbox specifies OS-level sandboxing (bwrap, sandbox-exec, jail) containment
	// settings for the extraction subprocess.
	Sandbox cmdutil.SandboxConfig
	// ExtraArgs holds additional user-specified command-line arguments
	// appended to every unrar invocation. Pre-validated by the config
	// layer to contain only flags starting with '-'.
	ExtraArgs []string
	// HasProblem is true when the detected unrar binary is too old (< 5.50)
	// or its version could not be determined. In this mode, flags that
	// old/non-RARLAB variants don't support are stripped:
	// -scf, -ai, -tsm-. Matches SABnzbd's RAR_PROBLEM degraded mode.
	HasProblem bool
	// UnrarVersion is the detected unrar version as UnrarInfo.Version
	// reports it (e.g. 712 for 7.12); 0 when unknown. From
	// unrarSkipSymlinksVersion on, UnRAR passes -ol- so unrar creates no
	// symlinks.
	UnrarVersion int
	// OnLine is called for each line of subprocess output. May be nil.
	OnLine func(string) `json:"-"`
	// OnCommand is called once per subprocess invocation, just before
	// exec, with the full display-safe command line (passwords redacted).
	OnCommand func(string) `json:"-"`

	// MinFreeSpace is the free space, in bytes, an extraction must leave on
	// the output directory's filesystem: downloads.min_free_space. An
	// archive whose headers declare more unpacked bytes than the free space
	// above it is refused before extraction starts (checkFreeSpace).
	MinFreeSpace int64
}

// unrarSkipSymlinksVersion is the first unrar with -ol-, which skips symbolic
// links when extracting (RAR 7.00 whatsnew, item 7). An older unrar rejects
// the switch as unknown and fails the extraction.
const unrarSkipSymlinksVersion = 700

// UnrarBinaries is the ordered list of unrar binary names tried during auto-detection.
var UnrarBinaries = []string{"unrar", "unrar-nonfree"}

// UnrarBin returns the path to the unrar binary to use.
func UnrarBin(opts Options) (string, error) {
	if opts.UnrarCommand != "" {
		return opts.UnrarCommand, nil
	}
	if env := os.Getenv("GONZBD_UNRAR_BIN"); env != "" {
		return env, nil
	}
	for _, name := range UnrarBinaries {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("unrar binary not found; install unrar, set GONZBD_UNRAR_BIN, or configure unrar_command")
}

// Result holds the outcome of an extraction attempt.
type Result struct {
	// CommandLine is the display-safe command line that was executed.
	// Passwords are redacted.
	CommandLine string
	// ExtractedFiles lists the files the extraction put in the output
	// directory. Every archive extractor lists what publishStagedExtraction
	// renamed out of its staging directory (extractInStage), relative to
	// the output directory. FileJoin lists the absolute path of the joined
	// file.
	ExtractedFiles []string
	// Output is the captured stdout+stderr of UnRAR's and SevenZip's
	// subprocess; GoUnRAR, GoSevenZip and GoTar fill it with one
	// "Extracting  <name>" line per extracted file.
	Output string
	// ExitCode is the process exit code (0 on success).
	ExitCode int
	// Reason classifies why the extraction failed (only meaningful when the
	// extractor returned an error, which is the one record of the failure).
	Reason FailReason
	// Engine names the extractor that produced this result, so callers can
	// label log output by the engine actually used rather than just the
	// archive type. Each extractor sets its own name on every return:
	// "unrar" (UnRAR), "7z" (SevenZip), "go_unrar" (GoUnRAR), "go_7z"
	// (GoSevenZip), "go_tar" (GoTar) and "filejoin" (FileJoin). Those are
	// its only writers: `git grep -n 'res.Engin[e] = ' -- 'internal/unpack/*.go'
	// ':!*_test.go'` returns 6 lines, one in each.
	Engine string
}

// UnRAR extracts archive.MainFile into outDir by shelling out to the `unrar`
// binary.  ctx cancellation kills the subprocess. unrar writes into a
// staging directory inside outDir, and publishStagedExtraction moves the
// result into outDir once the run succeeds.
//
// Argv construction:
//
//	unrar <mode> -y -idp [-scf -ai] -p<pw>|-p- -o+|-o- [-or] [-tsm-] [-ol-] [ExtraArgs] <mainfile> <stagedir>/
//
// where mode is 'e' (flat extract) when opts.OneFolder is true, or 'x'
// (preserve paths) otherwise.  -p- suppresses the interactive password
// prompt when no password is supplied, preventing the subprocess from
// blocking on stdin. The bracketed flags depend on opts (see HasProblem,
// IgnoreUnrarDates, UnrarVersion and ExtraArgs). The command line logged and
// passed to OnLine/OnCommand names <outdir>/ in place of <stagedir>/.
//
// A password that the archive's own check value proves wrong (rarPreVerify)
// is refused with FailWrongPassword without running unrar at all.
func UnRAR(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, opts Options) (Result, error) {
	res, err := unRAR(ctx, log.With("component", "unrar"), archive, outDir, password, opts)
	res.Engine = "unrar"
	return res, err
}

// errPreVerifiedWrong is returned for a password the archive's own check
// value proves wrong, which UnRAR refuses without running unrar.
var errPreVerifiedWrong = errors.New("unrar: password does not match the archive's check value")

func unRAR(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, opts Options) (Result, error) {
	if password != "" && rarPreVerify(archive, password) {
		log.Info("unrar: skipping known-wrong password (pre-verified)", "archive", archive.MainFile)
		if opts.OnLine != nil {
			opts.OnLine("Skipping known-wrong password (pre-verified)")
		}
		return Result{Reason: FailWrongPassword}, errPreVerifiedWrong
	}
	mode := "x"
	if opts.OneFolder {
		mode = "e"
	}

	argFlag := "-p-" // suppress interactive prompt
	if password != "" {
		argFlag = "-p<redacted>"
	}

	args := []string{
		mode,
		"-y",   // assume yes to all prompts
		"-idp", // disable progress display
	}

	// Normal mode (original unrar >= 5.50): add flags that free/old unrar
	// doesn't support. Matches SABnzbd's RAR_PROBLEM conditional logic.
	if !opts.HasProblem {
		args = append(args,
			"-scf", // assume UTF-8 filenames (§8.2 — fixes mojibake from Windows-built archives)
			"-ai",  // ignore file attributes — prevents read-only extracted files
		)
	}

	flagIdx := len(args)
	args = append(args, argFlag)

	// Extraction flags from config. Under OneFolder, -or renames a member
	// whose flattened name an earlier member took, as "name(1).ext", where
	// -o- alone would skip it. A degraded unrar is not passed -or, as
	// SABnzbd does not pass it to one.
	switch {
	case opts.OverwriteFiles:
		args = append(args, "-o+")
	case opts.OneFolder && !opts.HasProblem:
		args = append(args, "-o-", "-or")
	default:
		args = append(args, "-o-") // skip existing files
	}
	if opts.IgnoreUnrarDates && !opts.HasProblem {
		args = append(args, "-tsm-") // don't restore modification times (not supported by free unrar)
	}
	if opts.UnrarVersion >= unrarSkipSymlinksVersion {
		args = append(args, "-ol-") // create no symlinks; publishStagedExtraction drops any an older unrar makes
	}
	args = append(args, opts.ExtraArgs...) // user-specified extra flags

	displayArgs := append(append([]string(nil), args...), archive.MainFile, outDir+"/") // unrar expects a trailing slash on the output directory

	bin, err := UnrarBin(opts)
	if err != nil {
		return Result{}, err
	}

	// Build a display-safe command line (redact password).
	cmdLine := formatCmdLine(bin, displayArgs, "")

	log.Info("unrar: starting extraction",
		"binary", bin,
		"args", formatArgs(displayArgs, ""),
		"archive", archive.MainFile,
		"outDir", outDir,
		"cmdline", cmdLine,
	)

	// Emit full command line to the OnLine callback so it appears in the UI.
	if opts.OnLine != nil {
		opts.OnLine("Command: " + cmdLine)
	}
	if opts.OnCommand != nil {
		opts.OnCommand(cmdLine)
	}

	res, err := extractInStage(ctx, log, archive, outDir, password, opts, func(stageDir string) (Result, error) {
		execArgs := append(append([]string(nil), args...), archive.MainFile, stageDir+"/")
		if password != "" {
			execArgs[flagIdx] = "-p" + password
		}
		return runUnrar(ctx, log, archive, stageDir, bin, execArgs, opts)
	})
	res.CommandLine = cmdLine
	if err != nil {
		return res, err
	}
	log.Info("unrar: extraction succeeded", "archive", archive.MainFile)
	return res, nil
}

// runUnrar runs one unrar invocation that extracts into stageDir, and reports
// whether what it left there is to be published.
func runUnrar(ctx context.Context, log *slog.Logger, archive Archive, stageDir, bin string, execArgs []string, opts Options) (Result, error) {
	cmd, err := cmdutil.BuildSandboxedCommand(ctx, log, opts.CmdCfg, opts.Sandbox, bin, execArgs...) //nolint:gosec // args are caller-supplied, not shell-expanded
	if err != nil {
		return Result{Reason: FailUnknown}, err
	}
	streamer := cmdutil.NewLineStreamer(opts.OnLine)
	cmd.Stdout = streamer
	cmd.Stderr = streamer

	runErr := cmd.Run()
	streamer.Flush()

	res := Result{Output: streamer.String()}
	if runErr == nil {
		return res, nil
	}
	exitErr, ok := errors.AsType[*exec.ExitError](runErr)
	if !ok {
		log.Error("unrar: failed to start process",
			"archive", archive.MainFile,
			"err", runErr,
		)
		return res, fmt.Errorf("unrar: %w", runErr)
	}
	res.ExitCode = exitErr.ExitCode()
	res.Reason = ClassifyUnrarOutput(res.Output)

	// N4: When unrar prints "Cannot create" followed by "Attempting to
	// correct the filename", it auto-fixes invalid filenames. In this case
	// the extraction usually succeeds despite the non-zero exit code.
	// Downgrade to a warning. Matches SABnzbd's handling in newsunpack.py.
	//
	// A stage that cannot be read cannot be checked for corrected files, so
	// the extraction stays failed and the read error is reported with the
	// exit status.
	var snapErr error
	if strings.Contains(res.Output, "Cannot create") &&
		strings.Contains(res.Output, "Attempting to correct") {
		var stageSnap map[string]struct{}
		stageSnap, snapErr = snapshotDir(stageDir)
		if snapErr == nil && len(stageSnap) > 0 {
			log.Warn("unrar: auto-corrected invalid filename(s)",
				"archive", archive.MainFile,
				"stagedCount", len(stageSnap),
			)
			res.ExitCode = 0
			res.Reason = FailUnknown
			return res, nil
		}
	}
	err = fmt.Errorf("unrar exited %d (%s): %w", res.ExitCode, res.Reason, runErr)
	if snapErr != nil {
		err = errors.Join(err, fmt.Errorf("unrar: read staged files: %w", snapErr))
	}
	log.Error("unrar: extraction failed",
		"archive", archive.MainFile,
		"exitCode", res.ExitCode,
		"reason", res.Reason.String(),
		"output", res.Output,
		"err", err,
	)
	return res, err
}
