package unpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
)

// Options controls how an extraction tool is invoked.
type Options struct {
	// Password is the archive password.  Empty means no password.
	// Deprecated: use Passwords for multi-password iteration.
	Password string
	// Passwords is a prioritized list of passwords to try. When an
	// extraction fails with a "wrong password" error, the next password
	// in the list is tried. This implements SABnzbd's get_all_passwords()
	// behavior. The single Password field is appended as a fallback.
	Passwords []string
	// OneFolder extracts all files flat into outDir (no path preservation).
	// For unrar this selects the 'e' command; for 7zz it has no effect because
	// 7zz 'x' is used unconditionally and path preservation is the default.
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
	// IgnoreUnrarDates discards in-archive timestamps and uses extraction time.
	// Adds -ts0 to unrar arguments.
	IgnoreUnrarDates bool
	// Prefer7zip uses 7z instead of unrar for RAR extraction even when
	// unrar is available. 7z often handles edge-case RARs more reliably.
	Prefer7zip bool
}

// unrarBin returns the configured unrar binary, defaulting to "unrar".
func (o Options) unrarBin() string {
	if o.UnrarCommand != "" {
		return o.UnrarCommand
	}
	return "unrar"
}

// Result holds the outcome of an extraction attempt.
type Result struct {
	// ExtractedFiles is a best-effort list of files written by the tool.
	// TODO: populate by diffing outDir before/after the subprocess call.
	ExtractedFiles []string
	// Output is the captured stdout+stderr from the subprocess.
	Output string
	// ExitCode is the process exit code (0 on success).
	ExitCode int
	// Err is non-nil when the extraction failed.
	Err error
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
		pwFlag,
	}

	// Extraction flags from config.
	if opts.OverwriteFiles {
		args = append(args, "-o+")
	} else {
		args = append(args, "-o-")
	}
	if opts.IgnoreUnrarDates {
		args = append(args, "-ts0") // discard in-archive timestamps
	}

	args = append(args, archive.MainFile, outDir+"/") // unrar expects a trailing slash on the output directory

	bin := opts.unrarBin()

	log.Info("unrar: starting extraction",
		"binary", bin,
		"archive", archive.MainFile,
		"outDir", outDir,
		"oneFolder", opts.OneFolder,
		"hasPassword", opts.Password != "",
	)

	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // args are caller-supplied, not shell-expanded
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	runErr := cmd.Run()

	res := Result{
		Output: combined.String(),
	}

	var exitErr *exec.ExitError
	if runErr != nil {
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			res.Err = fmt.Errorf("unrar exited %d: %w", res.ExitCode, runErr)
			log.Error("unrar: extraction failed",
				"archive", archive.MainFile,
				"exitCode", res.ExitCode,
				"output", res.Output,
			)
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
