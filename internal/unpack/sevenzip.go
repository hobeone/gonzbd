package unpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
)

// sevenZipBin returns the path to the 7-zip binary to use.
// Resolution order:
//  1. SABNZBD_SEVENZIP_BIN environment variable (if set and non-empty).
//  2. "7zz" (preferred upstream binary name).
//  3. "7z" (common distro package name).
//
// Returns an error if neither binary is found in PATH.
func sevenZipBin() (string, error) {
	if env := os.Getenv("SABNZBD_SEVENZIP_BIN"); env != "" {
		return env, nil
	}
	if path, err := exec.LookPath("7zz"); err == nil {
		return path, nil
	}
	if path, err := exec.LookPath("7z"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("7-zip binary not found; install 7zz or 7z, or set SABNZBD_SEVENZIP_BIN")
}

// SevenZip extracts archive.MainFile into outDir by shelling out to the 7-zip
// binary (resolved via sevenZipBin).  ctx cancellation kills the subprocess.
//
// Argv construction:
//
//	7zz x -y -bso0 -bsp0 -p<pw> <mainfile> -o<outdir>
//
// The 'x' command is used unconditionally because it preserves directory
// structure; 'e' would flatten paths.  -bso0 and -bsp0 suppress output and
// progress streams to keep the captured output clean.  -p with an empty value
// is safe for 7zz — it does not prompt on stdin when -p is supplied.
func SevenZip(ctx context.Context, archive Archive, outDir string, opts Options) (Result, error) {
	bin, err := sevenZipBin()
	if err != nil {
		return Result{Err: err}, err
	}

	pwFlag := "-p" + opts.Password // safe even when Password is ""

	args := []string{
		"x",
		"-y",    // assume yes
		"-bso0", // suppress standard output stream
		"-bsp0", // suppress progress stream
		pwFlag,
		archive.MainFile,
		"-o" + outDir, // no space between -o and the path
	}

	slog.Info("7zip: starting extraction",
		"binary", bin,
		"archive", archive.MainFile,
		"outDir", outDir,
		"hasPassword", opts.Password != "",
	)

	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin and args are caller-supplied, not shell-expanded
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
			res.Err = fmt.Errorf("7zip exited %d: %w", res.ExitCode, runErr)
			slog.Error("7zip: extraction failed",
				"archive", archive.MainFile,
				"exitCode", res.ExitCode,
				"output", res.Output,
			)
		} else {
			res.Err = fmt.Errorf("7zip: %w", runErr)
			slog.Error("7zip: failed to start process",
				"archive", archive.MainFile,
				"err", runErr,
			)
			return res, res.Err
		}
		return res, res.Err
	}

	slog.Info("7zip: extraction succeeded", "archive", archive.MainFile)
	return res, nil
}
