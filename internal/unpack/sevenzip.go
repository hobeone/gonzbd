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

// SevenZipBinaries is the ordered list of 7-zip binary names tried during auto-detection.
var SevenZipBinaries = []string{"7zz", "7zzs", "7z", "7za"}

// SevenZipBin returns the path to the 7-zip binary to use.
// Resolution order:
//  1. Explicit command from Options (SevenZipCommand).
//  2. GONZBD_SEVENZIP_BIN environment variable (if set and non-empty).
//  3. "7zz" (preferred upstream binary name).
//  4. "7zzs" (static build — important for Alpine/musl).
//  5. "7z" (full 7-Zip — includes RAR5 codec, common distro package).
//  6. "7za" (standalone 7-Zip — lacks RAR codec, last resort).
//
// Returns an error if no binary is found. Callers in other packages should
// use this instead of reimplementing the resolution order, so the candidate
// list and env var only need to be kept in one place.
func SevenZipBin(opts Options) (string, error) {
	if opts.SevenZipCommand != "" {
		return opts.SevenZipCommand, nil
	}
	if env := os.Getenv("GONZBD_SEVENZIP_BIN"); env != "" {
		return env, nil
	}
	for _, name := range SevenZipBinaries {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("7-zip binary not found; install 7zz or 7z, set GONZBD_SEVENZIP_BIN, or configure sevenz_command")
}

// SevenZip extracts archive.MainFile into outDir by shelling out to the 7-zip
// binary (resolved via SevenZipBin).  ctx cancellation kills the subprocess.
// 7z writes into a staging directory inside outDir, and
// publishStagedExtraction moves the result into outDir once the run succeeds.
//
// Argv construction:
//
//	7zz x|e -y -bd -bsp0 -p<pw> -aoa|-aou|-aos <mainfile> -o<stagedir> -ssc|-ssc-
//
// 'x' preserves directory structure; 'e' flattens paths (when opts.OneFolder
// is true, matching SABnzbd's cfg.flat_unpack).  -bsp0 suppresses the
// progress stream.  -p with an empty value is safe for 7zz — it does not
// prompt on stdin when -p is supplied. The command line logged and passed to
// OnLine/OnCommand names -o<outdir> in place of -o<stagedir>.
func SevenZip(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, opts Options) (Result, error) {
	res, err := sevenZip(ctx, log.With("component", "sevenzip"), archive, outDir, password, opts)
	res.Engine = "7z"
	return res, err
}

func sevenZip(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, opts Options) (Result, error) {
	bin, err := SevenZipBin(opts)
	if err != nil {
		return Result{}, err
	}

	argFlag := "-p"
	if password != "" {
		argFlag = "-p<redacted>"
	}

	method := "x" // preserve paths
	if opts.OneFolder {
		method = "e" // flat extract (matches SABnzbd cfg.flat_unpack)
	}

	args := []string{
		method,
		"-y",    // assume yes
		"-bd",   // disable progress percentage indicator
		"-bsp0", // suppress progress stream (keep stdout for stage log)
	}
	flagIdx := len(args)
	args = append(args, argFlag)

	// Overwrite behavior. Under OneFolder, -aou renames a member whose
	// flattened name an earlier member took, as "name_1.ext", where -aos
	// would skip it.
	switch {
	case opts.OverwriteFiles:
		args = append(args, "-aoa") // overwrite all existing files (SABnzbd default)
	case opts.OneFolder:
		args = append(args, "-aou")
	default:
		args = append(args, "-aos") // skip extracting of existing files
	}

	// P11: Case-sensitivity flag. Linux filesystems are case-sensitive;
	// macOS/Windows are not. Matches Python SABnzbd spec §8.3.
	sscFlag := "-ssc-" // case-insensitive
	if runtime.GOOS == "linux" {
		sscFlag = "-ssc" // case-sensitive
	}

	displayArgs := append(append([]string(nil), args...), archive.MainFile, "-o"+outDir, sscFlag)

	// Build a display-safe command line (redact password).
	cmdLine := formatCmdLine(bin, displayArgs, "")

	log.Info("7zip: starting extraction",
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
		execArgs := append(append([]string(nil), args...), archive.MainFile, "-o"+stageDir, sscFlag)
		if password != "" {
			execArgs[flagIdx] = "-p" + password
		}
		return runSevenZip(ctx, log, archive, bin, execArgs, opts)
	})
	res.CommandLine = cmdLine
	if err != nil {
		return res, err
	}
	log.Info("7zip: extraction succeeded", "archive", archive.MainFile)
	return res, nil
}

// runSevenZip runs one 7z invocation, whose argv names the stage it extracts
// into.
func runSevenZip(ctx context.Context, log *slog.Logger, archive Archive, bin string, execArgs []string, opts Options) (Result, error) {
	cmd, err := cmdutil.BuildSandboxedCommand(ctx, log, opts.CmdCfg, opts.Sandbox, bin, execArgs...) //nolint:gosec // bin and args are caller-supplied, not shell-expanded
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
		log.Error("7zip: failed to start process",
			"archive", archive.MainFile,
			"err", runErr,
		)
		return res, fmt.Errorf("7zip: %w", runErr)
	}
	res.ExitCode = exitErr.ExitCode()
	res.Reason = Classify7zOutput(res.Output)
	log.Error("7zip: extraction failed",
		"archive", archive.MainFile,
		"exitCode", res.ExitCode,
		"reason", res.Reason.String(),
		"output", res.Output,
	)
	return res, fmt.Errorf("7zip exited %d (%s): %w", res.ExitCode, res.Reason, runErr)
}
