// Package par2 provides functions to find, verify, and repair par2 sets by
// shelling out to the par2 command-line binary.
package par2

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

// Status represents the outcome of a par2 verify or repair operation.
type Status int

const (
	// StatusUnknown means the par2 output could not be classified.
	StatusUnknown Status = iota
	// StatusAllFilesOK means par2 reported all files are correct.
	StatusAllFilesOK
	// StatusRepairRequired means par2 detected damage and repair is needed.
	StatusRepairRequired
	// StatusRepairPossible means par2 reports enough recovery data to repair.
	StatusRepairPossible
	// StatusRepairNotPossible means damage exceeds available recovery blocks.
	StatusRepairNotPossible
	// StatusNeedMoreBlocks means par2 needs additional recovery blocks to repair.
	// This triggers the re-add path: the job is pushed back to the download queue
	// to fetch additional par2 volume files.
	StatusNeedMoreBlocks
	// StatusInvalidPar2 means the par2 file itself is missing or corrupt.
	StatusInvalidPar2
	// StatusDiskFull means par2 reported insufficient disk space.
	StatusDiskFull
	// StatusRepairFailed means par2 attempted repair but it was unsuccessful.
	StatusRepairFailed
	// StatusRepairComplete means par2 successfully repaired all damaged files.
	StatusRepairComplete
	// StatusBadOption means par2 rejected a command-line option (e.g. turbo
	// flags passed to a non-turbo build). The caller should retry without
	// the problematic option.
	StatusBadOption
)

// Set groups a collection of par2 files that belong to the same repair set.
// The set is keyed by the basename minus any trailing .volNN+MM suffix.
type Set struct {
	// Name is the base name of the set (e.g. "movie" for "movie.par2").
	Name string
	// MainFile is the non-volume .par2 file (e.g. "movie.par2").  May be
	// empty if the directory contains only volume files.
	MainFile string
	// ExtraFiles holds the volume .par2 files (e.g. "movie.vol000+01.par2").
	ExtraFiles []string
}

// ParseFile returns the par2 file to parse for this set: the main
// (non-volume) file when present, otherwise the first volume file. It
// returns "" when the set has no .par2 files at all. Callers that need the
// set's file descriptions resolve the path through this method so the
// main-or-first-volume fallback lives in one place.
func (s Set) ParseFile() string {
	if s.MainFile != "" {
		return s.MainFile
	}
	if len(s.ExtraFiles) > 0 {
		return s.ExtraFiles[0]
	}
	return ""
}

// VerifyResult holds the output of a par2 verify run.
type VerifyResult struct {
	// CommandLine is the full command that was executed.
	CommandLine string
	// Status is the parsed outcome.
	Status Status
	// Stdout is the raw standard output captured from par2.
	Stdout string
	// Stderr is the raw standard error captured from par2.
	Stderr string
}

// RepairResult holds the output of a par2 repair run.
type RepairResult struct {
	// CommandLine is the full command that was executed.
	CommandLine string
	// Success is true when par2 reported "Repair complete" or "All files are correct".
	Success bool
	// ExitCode is the process exit code; 0 on success.
	ExitCode int
	// Output is the combined stdout and stderr from par2.
	Output string
	// NeedMoreBlocks is true when par2 reported "You need N more recovery blocks".
	// This indicates the job should be requeued to fetch additional par2 volumes.
	NeedMoreBlocks bool
	// BlocksNeeded is the number of additional recovery blocks par2 requires.
	// Only meaningful when NeedMoreBlocks is true.
	BlocksNeeded int
	// Parsed is the fully parsed output state from par2. Contains renames,
	// used_for_repair/used_joinables, progress, and phase tracking.
	// Nil if output parsing was not performed.
	Parsed *RepairOutput
}

// RunOptions configures the par2 binary invocation.
type RunOptions struct {
	// Command is the path to the par2 binary. Empty defaults to "par2".
	Command string
	// Turbo enables par2cmdline-turbo arguments (-t+) for faster
	// multi-threaded repair when the binary supports them.
	Turbo bool
	// CmdCfg controls nice/ionice process priority wrapping.
	// Matches SABnzbd's cfg.nice/cfg.ionice.
	CmdCfg cmdutil.CmdConfig
	// Sandbox controls bubblewrap sandboxing for par2 execution.
	Sandbox cmdutil.SandboxConfig
	// ExtraArgs holds additional user-specified command-line arguments
	// appended to every par2 invocation. Pre-validated at config load.
	ExtraArgs []string
	// Caps holds detected capabilities of the par2 binary (from
	// DetectCapabilities at startup). Used to conditionally add -N
	// and -B flags. May be nil when detection hasn't run.
	Caps *Caps
	// OnLine is called for each line of output from the par2 subprocess.
	// May be nil.
	OnLine func(string) `json:"-"`
	// OnCommand is called once per subprocess invocation, just before
	// exec, with the full command line that's about to run. Lets callers
	// log the actual argv. May be nil.
	OnCommand func(string) `json:"-"`
}

// Bin returns the configured par2 binary, defaulting to "par2".
func (o RunOptions) Bin() string {
	if o.Command != "" {
		return o.Command
	}
	return "par2"
}

// capsArgs returns the conditional flags based on detected binary
// capabilities. dir is the directory containing the par2 file.
func capsArgs(caps *Caps, dir string) []string {
	if caps == nil {
		return nil
	}
	var args []string
	if caps.SupportsNoDataSkipping {
		args = append(args, "-N")
	}
	if caps.SupportsBasepath && dir != "" {
		args = append(args, "-B", dir)
	}
	return args
}

// volPattern matches the volume suffix of a par2 filename, e.g. ".vol000+01" or ".vol03-07".
var volPattern = regexp.MustCompile(`(?i)\.vol\d+[-+]\d+$`)

// recoveryVolumePattern matches the .volNNN+MM.par2 or .volNNN-MM.par2 component anywhere in a
// string. It is intentionally more permissive about surrounding context than
// volPattern so that it works on raw NZB subject strings such as
//
//	[3/12] - "movie.vol000+01.par2" yEnc (1/42)
//
// where the filename is wrapped in yEnc tokens rather than appearing as a
// clean basename.  The word-boundary (\b) after ".par2" anchors on the quote,
// space, or end-of-string that follows in typical NZB subjects.
var recoveryVolumePattern = regexp.MustCompile(`(?i)\.vol\d+[-+]\d+\.par2(?:[^a-zA-Z0-9.]|$)`)

// IsRecoveryVolume reports whether s refers to a par2 recovery volume
// (e.g. "movie.vol000+01.par2") rather than the par2 index file
// (e.g. "movie.par2").
//
// s may be a raw NZB subject string such as
//
//	[3/12] - "movie.vol000+01.par2" yEnc (1/42)
//
// and does not need to be a clean filename.  This is the primary difference
// from the unexported isVolume helper, which strips the extension with
// filepath.Ext and therefore only works on a clean basename.
func IsRecoveryVolume(s string) bool {
	return recoveryVolumePattern.MatchString(s)
}

// needMoreBlocksRE extracts the block count from par2's "You need N more
// recovery blocks" message. Spec §7.3.
var needMoreBlocksRE = regexp.MustCompile(`You need (\d+) more recovery block`)

// setName derives the set name from a par2 filename (without directory).
// Examples:
//
//	"movie.par2"           → "movie"
//	"movie.vol000+01.par2" → "movie"
func setName(base string) string {
	// Strip the final ".par2" extension (case-insensitive handled by ToLower on ext).
	ext := filepath.Ext(base)
	if !strings.EqualFold(ext, ".par2") {
		return base
	}
	trimmed := base[:len(base)-len(ext)]
	// Strip any volume suffix.
	trimmed = volPattern.ReplaceAllString(trimmed, "")
	return trimmed
}

// isVolume returns true if the filename contains a .volNN+MM component.
func isVolume(name string) bool {
	return volPattern.MatchString(name[:len(name)-len(filepath.Ext(name))])
}

// FindPar2Files scans dir for .par2 files and groups them into Sets.
// Files that are not .par2 files are ignored.  Returns an empty slice (not an
// error) when no par2 files are found.  The returned slice is ordered by set
// name. Optional ParseOptions can be provided.
func FindPar2Files(dir string, opts ...ParseOptions) ([]Set, error) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("par2 find: %w", err)
	}

	var entries []string
	for _, de := range dirEntries {
		if !de.IsDir() && strings.EqualFold(filepath.Ext(de.Name()), ".par2") {
			entries = append(entries, filepath.Join(dir, de.Name()))
		}
	}

	byName := make(map[string]*Set)

	for _, path := range entries {
		base := filepath.Base(path)
		name := setName(base)

		s, ok := byName[name]
		if !ok {
			s = &Set{Name: name}
			byName[name] = s
		}

		if isVolume(base) {
			s.ExtraFiles = append(s.ExtraFiles, path)
		} else {
			s.MainFile = path
		}
	}

	result := make([]Set, 0, len(byName))
	for _, s := range byName {
		result = append(result, *s)
	}

	slices.SortFunc(result, func(a, b Set) int {
		return strings.Compare(a.Name, b.Name)
	})

	return result, nil
}

// parseStatus inspects par2 output lines and returns the most specific Status.
// Failure conditions are checked first so they take priority over success
// messages when both appear in the output (e.g. a partial repair attempt
// that prints "All files are correct" for some files but ultimately fails).
func parseStatus(output string) Status {
	switch {
	case strings.Contains(output, "Main packet not found"),
		strings.Contains(output, "The recovery file does not exist"):
		return StatusInvalidPar2
	case strings.Contains(output, "Invalid option specified"),
		strings.Contains(output, "Invalid thread option"):
		return StatusBadOption
	case strings.Contains(output, "not enough space on the disk"),
		strings.Contains(output, "Insufficient disk space"):
		return StatusDiskFull
	case needMoreBlocksRE.MatchString(output):
		return StatusNeedMoreBlocks
	case strings.Contains(output, "Repair Failed"):
		return StatusRepairFailed
	case strings.Contains(output, "Repair is not possible"):
		return StatusRepairNotPossible
	case strings.Contains(output, "cannot be renamed to"):
		return StatusRepairNotPossible
	case strings.Contains(output, "No details available for recoverable file"):
		return StatusRepairNotPossible
	case strings.Contains(output, "Repair is required"):
		return StatusRepairRequired
	case strings.Contains(output, "Repair is possible"):
		return StatusRepairPossible
	case strings.Contains(output, "Repair complete"):
		return StatusRepairComplete
	case strings.Contains(output, "All files are correct"):
		return StatusAllFilesOK
	default:
		return StatusUnknown
	}
}

// verify runs `par2 v <parfile> [extraFiles...]` and parses the output to
// determine whether the protected files are intact.  It returns a VerifyResult
// even when par2 itself exits with a non-zero code, as long as the process
// could be started. A returned error indicates a system-level failure (binary
// not found, context cancelled, etc.).
func verify(ctx context.Context, parfile string, extraFiles ...string) (VerifyResult, error) {
	return verifyWith(ctx, RunOptions{}, parfile, extraFiles...)
}

// sboxForParfile returns the SandboxConfig for running par2 on parfile.
// If opts.Sandbox.TargetDir is empty, it defaults TargetDir to the directory containing parfile.
// If opts.Sandbox.TargetDir is non-empty, it verifies that parfile is contained within TargetDir.
func sboxForParfile(opts RunOptions, parfile string) (cmdutil.SandboxConfig, error) {
	sbox := opts.Sandbox
	absPar, err := filepath.Abs(parfile)
	if err != nil {
		return sbox, fmt.Errorf("parfile abs path: %w", err)
	}

	if sbox.TargetDir == "" {
		sbox.TargetDir = filepath.Dir(absPar)
		return sbox, nil
	}

	absTarget, err := filepath.Abs(sbox.TargetDir)
	if err != nil {
		return sbox, fmt.Errorf("target dir abs path: %w", err)
	}
	sbox.TargetDir = absTarget

	rel, err := filepath.Rel(absTarget, absPar)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return sbox, fmt.Errorf("parfile %q escapes sandbox target directory %q", parfile, absTarget)
	}

	return sbox, nil
}

// verifyWith is like verify but uses the given RunOptions for binary selection.
func verifyWith(ctx context.Context, opts RunOptions, parfile string, extraFiles ...string) (VerifyResult, error) {
	streamer := cmdutil.NewLineStreamer(opts.OnLine)
	args := make([]string, 0, 6+len(extraFiles))
	args = append(args, "v", "-q")
	args = append(args, capsArgs(opts.Caps, filepath.Dir(parfile))...)
	args = append(args, opts.ExtraArgs...)
	args = append(args, parfile)
	args = append(args, extraFiles...)

	cmdLine := formatParCmdLine(opts.Bin(), args)
	if opts.OnLine != nil {
		opts.OnLine("Command: " + cmdLine)
	}
	if opts.OnCommand != nil {
		opts.OnCommand(cmdLine)
	}

	sbox, err := sboxForParfile(opts, parfile)
	if err != nil {
		return VerifyResult{CommandLine: cmdLine, Status: StatusUnknown}, fmt.Errorf("par2 verify sandbox: %w", err)
	}

	cmd, err := cmdutil.BuildSandboxedCommand(ctx, slog.Default(), opts.CmdCfg, sbox, opts.Bin(), args...) //nolint:gosec // parfile and extraFiles are caller-supplied, not shell-expanded
	if err != nil {
		return VerifyResult{CommandLine: cmdLine, Status: StatusUnknown}, fmt.Errorf("par2 verify: %w", err)
	}
	cmd.Dir = filepath.Dir(parfile)
	cmd.Stdout = streamer
	cmd.Stderr = streamer

	runErr := cmd.Run()
	streamer.Flush()

	output := streamer.String()
	res := VerifyResult{
		CommandLine: cmdLine,
		Stdout:      output,
		Stderr:      "", // combined into Stdout now
		Status:      parseStatus(output),
	}

	// Propagate only system-level errors, not par2's repair-required exit codes.
	if runErr != nil {
		if _, ok := errors.AsType[*exec.ExitError](runErr); !ok {
			return res, fmt.Errorf("par2 verify: %w", runErr)
		}
	}

	return res, nil
}

// repair runs `par2 r <parfile> [extraFiles...]` and attempts to repair any
// damaged files. Like verify, it returns a RepairResult even on non-zero exit
// codes.  A non-nil error signals a system-level failure.
func repair(ctx context.Context, parfile string, extraFiles ...string) (RepairResult, error) {
	return RepairWith(ctx, RunOptions{}, parfile, extraFiles...)
}

// RepairWith is like Repair but uses the given RunOptions for binary selection.
func RepairWith(ctx context.Context, opts RunOptions, parfile string, extraFiles ...string) (RepairResult, error) {
	streamer := cmdutil.NewLineStreamer(opts.OnLine)
	args := make([]string, 0, 6+len(extraFiles))
	// I7: Do NOT use -q for repair. Quiet mode suppresses File:/Target:
	// status lines that ParseRepairOutput needs for rename tracking,
	// used_joinables detection, and verification progress counting.
	// SABnzbd also does not use -q for par2. Keep -q on verify-only
	// calls (VerifyWith) where we only need the aggregate status.
	args = append(args, "r")
	args = append(args, capsArgs(opts.Caps, filepath.Dir(parfile))...)
	args = append(args, opts.ExtraArgs...)
	args = append(args, parfile)
	args = append(args, extraFiles...)

	cmdLine := formatParCmdLine(opts.Bin(), args)
	if opts.OnLine != nil {
		opts.OnLine("Command: " + cmdLine)
	}
	if opts.OnCommand != nil {
		opts.OnCommand(cmdLine)
	}

	sbox, err := sboxForParfile(opts, parfile)
	if err != nil {
		return RepairResult{CommandLine: cmdLine}, fmt.Errorf("par2 repair sandbox: %w", err)
	}

	cmd, err := cmdutil.BuildSandboxedCommand(ctx, slog.Default(), opts.CmdCfg, sbox, opts.Bin(), args...) //nolint:gosec // parfile and extraFiles are caller-supplied, not shell-expanded
	if err != nil {
		return RepairResult{CommandLine: cmdLine}, fmt.Errorf("par2 repair: %w", err)
	}
	cmd.Dir = filepath.Dir(parfile)
	cmd.Stdout = streamer
	cmd.Stderr = streamer

	runErr := cmd.Run()
	streamer.Flush()

	res := RepairResult{
		CommandLine: cmdLine,
		Output:      streamer.String(),
	}

	if runErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			res.ExitCode = exitErr.ExitCode()
		} else {
			return res, fmt.Errorf("par2 repair: %w", runErr)
		}
	}

	// Parse the combined output with the streaming state machine.
	parsed := ParseRepairOutput(res.Output)
	res.Parsed = parsed
	res.Success = parsed.Finished
	res.NeedMoreBlocks = parsed.NeedMoreBlocks
	res.BlocksNeeded = parsed.BlocksNeeded

	return res, nil
}
