package unpack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

// sevenZipBin returns the path to the 7-zip binary to use.
// Resolution order:
//  1. Explicit command from Options (SevenZipCommand).
//  2. GONZBD_SEVENZIP_BIN environment variable (if set and non-empty).
//  3. "7zz" (preferred upstream binary name).
//  4. "7zzs" (static build — important for Alpine/musl).
//  5. "7z" (full 7-Zip — includes RAR5 codec, common distro package).
//  6. "7za" (standalone 7-Zip — lacks RAR codec, last resort).
//
// Returns an error if no binary is found.
func sevenZipBin(opts Options) (string, error) {
	if opts.SevenZipCommand != "" {
		return opts.SevenZipCommand, nil
	}
	if env := os.Getenv("GONZBD_SEVENZIP_BIN"); env != "" {
		return env, nil
	}
	for _, name := range []string{"7zz", "7zzs", "7z", "7za"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("7-zip binary not found; install 7zz or 7z, set GONZBD_SEVENZIP_BIN, or configure sevenz_command")
}

// SevenZip extracts archive.MainFile into outDir by shelling out to the 7-zip
// binary (resolved via sevenZipBin).  ctx cancellation kills the subprocess.
//
// Argv construction:
//
//	7zz x|e -y -bsp0 -p<pw> <mainfile> -o<outdir>
//
// 'x' preserves directory structure; 'e' flattens paths (when opts.OneFolder
// is true, matching SABnzbd's cfg.flat_unpack).  -bsp0 suppresses the
// progress stream.  -p with an empty value is safe for 7zz — it does not
// prompt on stdin when -p is supplied.
func SevenZip(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
	log = log.With("component", "unpack/sevenzip")
	bin, err := sevenZipBin(opts)
	if err != nil {
		return Result{Err: err}, err
	}

	pwFlag := "-p" + opts.Password // safe even when Password is ""

	method := "x" // preserve paths
	if opts.OneFolder {
		method = "e" // flat extract (matches SABnzbd cfg.flat_unpack)
	}

	args := []string{
		method,
		"-y",    // assume yes
		"-bd",   // disable progress percentage indicator
		"-bsp0", // suppress progress stream (keep stdout for stage log)
		pwFlag,
	}

	// Overwrite behavior.
	if opts.OverwriteFiles {
		args = append(args, "-aoa") // overwrite all existing files
	} else {
		args = append(args, "-aou") // auto-rename on collision (matches SABnzbd)
	}

	args = append(args,
		archive.MainFile,
		"-o"+outDir, // no space between -o and the path
	)

	// P11: Case-sensitivity flag. Linux filesystems are case-sensitive;
	// macOS/Windows are not. Matches Python SABnzbd spec §8.3.
	if runtime.GOOS == "linux" {
		args = append(args, "-ssc") // case-sensitive
	} else {
		args = append(args, "-ssc-") // case-insensitive
	}

	// Build a display-safe command line (redact password).
	cmdLine := formatCmdLine(bin, args, pwFlag)

	log.Info("7zip: starting extraction",
		"binary", bin,
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

	cmd := cmdutil.BuildCommand(ctx, opts.CmdCfg, bin, args...) //nolint:gosec // bin and args are caller-supplied, not shell-expanded
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

	var exitErr *exec.ExitError
	if runErr != nil {
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			res.Reason = Classify7zOutput(res.Output)
			res.Err = fmt.Errorf("7zip exited %d (%s): %w", res.ExitCode, res.Reason, runErr)
			log.Error("7zip: extraction failed",
				"archive", archive.MainFile,
				"exitCode", res.ExitCode,
				"reason", res.Reason.String(),
				"output", res.Output,
			)
		} else {
			res.Err = fmt.Errorf("7zip: %w", runErr)
			log.Error("7zip: failed to start process",
				"archive", archive.MainFile,
				"err", runErr,
			)
			return res, res.Err
		}
		return res, res.Err
	}

	log.Info("7zip: extraction succeeded", "archive", archive.MainFile)
	return res, nil
}
