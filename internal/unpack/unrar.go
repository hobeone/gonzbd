package unpack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

// Options controls how an extraction tool is invoked.
type Options struct {
	// Password is the password used for a single extraction attempt. Empty
	// means no password. UnRAR, SevenZip, GoUnRAR, and GoSevenZip read this
	// field directly. Callers that want multi-password iteration should set
	// Passwords instead and call the corresponding *WithPasswords wrapper,
	// which populates Password per attempt from the candidate list.
	Password string
	// Passwords is a prioritized list of passwords to try. When an
	// extraction fails with a "wrong password" error, the next password
	// in the list is tried. This implements SABnzbd's get_all_passwords()
	// behavior. The single Password field is appended as a fallback
	// candidate.
	Passwords []string
	// OneFolder extracts all files flat into outDir (no path preservation).
	// For unrar this selects the 'e' command; for 7z this selects 'e'
	// instead of 'x'.
	OneFolder bool
	// KeepOriginals controls whether the caller should delete the source Parts
	// after a successful extraction.  The unpack functions themselves never
	// delete files; deletion is the caller's responsibility.
	KeepOriginals bool

	// UnrarCommand overrides the unrar binary path. Empty defaults to "unrar".
	UnrarCommand string
	// SevenZipCommand overrides the 7z binary path. Empty uses auto-detection.
	SevenZipCommand string
	// OverwriteFiles controls whether extraction overwrites existing files.
	// true = overwrite (-o+ for unrar, -aoa for 7z); false = skip (-o- for unrar, -aos for 7z).
	// Default (false) preserves existing files.
	OverwriteFiles bool
	// IgnoreUnrarDates discards in-archive modification timestamps and uses extraction time.
	// Adds -tsm- to unrar arguments (matches SABnzbd's behavior).
	IgnoreUnrarDates bool
	// UseGoRAR uses the pure-Go rarengine library for RAR3/RAR5 extraction
	// instead of shelling out to unrar. No external binary required.
	// Default true.
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
	// ExtraArgs holds additional user-specified command-line arguments
	// appended to every unrar invocation. Pre-validated by the config
	// layer to contain only flags starting with '-'.
	ExtraArgs []string
	// HasProblem is true when the detected unrar binary is too old (< 5.50)
	// or its version could not be determined. In this mode, flags that
	// old/non-RARLAB variants don't support are stripped:
	// -scf, -or, -ai, -tsm-. Matches SABnzbd's RAR_PROBLEM degraded mode.
	HasProblem bool
	// OnLine is called for each line of subprocess output. May be nil.
	OnLine func(string) `json:"-"`
	// OnCommand is called once per subprocess invocation, just before
	// exec, with the full display-safe command line (passwords redacted).
	// Lets callers log the actual argv that's about to run. May be nil.
	OnCommand func(string) `json:"-"`
}

// UnrarBin returns the configured unrar binary, defaulting to "unrar".
func (o Options) UnrarBin() string {
	if o.UnrarCommand != "" {
		return o.UnrarCommand
	}
	return "unrar"
}

// Result holds the outcome of an extraction attempt.
type Result struct {
	// CommandLine is the display-safe command line that was executed.
	// Passwords are redacted.
	CommandLine string
	// ExtractedFiles lists files created by the extraction, determined by
	// diffing the output directory before and after the subprocess call.
	ExtractedFiles []string
	// Output is the captured stdout+stderr from the subprocess.
	Output string
	// ExitCode is the process exit code (0 on success).
	ExitCode int
	// Reason classifies why the extraction failed (only meaningful when Err != nil).
	Reason FailReason
	// Err is non-nil when the extraction failed.
	Err error
	// Engine identifies which extractor produced this result (e.g. "unrar",
	// "go_unrar", "7z", "go_7z"), so callers can label log output by the
	// engine actually used rather than just the archive type.
	Engine string
}

// UnRAR extracts archive.MainFile into outDir by shelling out to the `unrar`
// binary.  ctx cancellation kills the subprocess.
//
// Argv construction:
//
//	unrar <mode> -y -idp -p<pw>|-p- <mainfile> <outdir>/
//
// where mode is 'e' (flat extract) when opts.OneFolder is true, or 'x'
// (preserve paths) otherwise.  -p- suppresses the interactive password
// prompt when no password is supplied, preventing the subprocess from
// blocking on stdin.
func UnRAR(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
	log = log.With("component", "unpack/unrar")
	mode := "x"
	if opts.OneFolder {
		mode = "e"
	}

	pwFlag := "-p-" // suppress interactive prompt
	if opts.Password != "" {
		pwFlag = "-p" + opts.Password
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

	args = append(args, pwFlag)

	// Extraction flags from config.
	if opts.OverwriteFiles {
		args = append(args, "-o+")
	} else {
		args = append(args, "-o-") // don't overwrite existing files
		if !opts.HasProblem {
			args = append(args, "-or") // auto-rename on collision (not supported by free unrar)
		}
	}
	if opts.IgnoreUnrarDates && !opts.HasProblem {
		args = append(args, "-tsm-") // don't restore modification times (not supported by free unrar)
	}
	args = append(args, opts.ExtraArgs...) // user-specified extra flags

	args = append(args, archive.MainFile, outDir+"/") // unrar expects a trailing slash on the output directory

	bin := opts.UnrarBin()

	// Build a display-safe command line (redact password).
	cmdLine := formatCmdLine(bin, args, pwFlag)

	log.Info("unrar: starting extraction",
		"binary", bin,
		"args", args,
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

	cmd := cmdutil.BuildCommand(ctx, opts.CmdCfg, bin, args...) //nolint:gosec // args are caller-supplied, not shell-expanded
	streamer := cmdutil.NewLineStreamer(opts.OnLine)
	cmd.Stdout = streamer
	cmd.Stderr = streamer

	// Snapshot directory contents before extraction for diff.
	beforeSnap, _ := snapshotDir(outDir)

	runErr := cmd.Run()
	streamer.Flush()

	// Diff to find newly created files (best-effort).
	afterSnap, _ := snapshotDir(outDir)
	extracted := diffSnapshot(beforeSnap, afterSnap)

	res := Result{
		CommandLine:    cmdLine,
		Output:         streamer.String(),
		ExtractedFiles: extracted,
	}

	if runErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			res.ExitCode = exitErr.ExitCode()
			res.Reason = ClassifyUnrarOutput(res.Output)

			// N4: When unrar prints "Cannot create" followed by
			// "Attempting to correct the filename", it auto-fixes
			// invalid filenames. In this case the extraction usually
			// succeeds despite the non-zero exit code. Downgrade to
			// a warning. Matches SABnzbd's handling in newsunpack.py.
			if strings.Contains(res.Output, "Cannot create") &&
				strings.Contains(res.Output, "Attempting to correct") &&
				len(extracted) > 0 {
				log.Warn("unrar: auto-corrected invalid filename(s)",
					"archive", archive.MainFile,
					"extractedCount", len(extracted),
				)
				// Clear the error — extraction succeeded with corrections.
				res.Err = nil
				res.ExitCode = 0
			} else {
				res.Err = fmt.Errorf("unrar exited %d (%s): %w", res.ExitCode, res.Reason, runErr)
				log.Error("unrar: extraction failed",
					"archive", archive.MainFile,
					"exitCode", res.ExitCode,
					"reason", res.Reason.String(),
					"output", res.Output,
				)
			}
		} else {
			res.Err = fmt.Errorf("unrar: %w", runErr)
			log.Error("unrar: failed to start process",
				"archive", archive.MainFile,
				"err", runErr,
			)
			return res, res.Err
		}
		return res, res.Err
	}

	log.Info("unrar: extraction succeeded", "archive", archive.MainFile)
	return res, nil
}
