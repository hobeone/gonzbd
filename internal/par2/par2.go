// Package par2 provides functions to find, verify, and repair par2 sets by
// shelling out to the par2 command-line binary.
package par2

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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

// VerifyResult holds the output of a par2 verify run.
type VerifyResult struct {
	// Status is the parsed outcome.
	Status Status
	// Stdout is the raw standard output captured from par2.
	Stdout string
	// Stderr is the raw standard error captured from par2.
	Stderr string
}

// RepairResult holds the output of a par2 repair run.
type RepairResult struct {
	// Success is true when par2 reported "Repair complete".
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
}

// RunOptions configures the par2 binary invocation.
type RunOptions struct {
	// Command is the path to the par2 binary. Empty defaults to "par2".
	Command string
	// Turbo enables par2cmdline-turbo arguments (-t+) for faster
	// multi-threaded repair when the binary supports them.
	Turbo bool
	// OnLine is called for each line of output from the par2 subprocess.
	// May be nil.
	OnLine func(string) `json:"-"`
}

// command returns the configured par2 binary, defaulting to "par2".
func (o RunOptions) command() string {
	if o.Command != "" {
		return o.Command
	}
	return "par2"
}

// volPattern matches the volume suffix of a par2 filename, e.g. ".vol000+01".
var volPattern = regexp.MustCompile(`(?i)\.vol\d+\+\d+$`)

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
// name.
func FindPar2Files(dir string) ([]Set, error) {
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
	case strings.Contains(output, "not enough space on the disk"),
		strings.Contains(output, "Insufficient disk space"):
		return StatusDiskFull
	case needMoreBlocksRE.MatchString(output):
		return StatusNeedMoreBlocks
	case strings.Contains(output, "Repair is not possible"):
		return StatusRepairNotPossible
	case strings.Contains(output, "cannot be renamed to"):
		return StatusRepairNotPossible
	case strings.Contains(output, "Repair is required"):
		return StatusRepairRequired
	case strings.Contains(output, "Repair is possible"):
		return StatusRepairPossible
	case strings.Contains(output, "All files are correct"):
		return StatusAllFilesOK
	default:
		return StatusUnknown
	}
}

// Verify runs `par2 v <parfile> [extraFiles...]` and parses the output to
// determine whether the protected files are intact.  It returns a VerifyResult
// even when par2 itself exits with a non-zero code, as long as the process
// could be started. A returned error indicates a system-level failure (binary
// not found, context cancelled, etc.).
func Verify(ctx context.Context, parfile string, extraFiles ...string) (VerifyResult, error) {
	return VerifyWith(ctx, RunOptions{}, parfile, extraFiles...)
}

// VerifyWith is like Verify but uses the given RunOptions for binary selection.
func VerifyWith(ctx context.Context, opts RunOptions, parfile string, extraFiles ...string) (VerifyResult, error) {
	streamer := cmdutil.NewLineStreamer(opts.OnLine)
	args := make([]string, 0, 3+len(extraFiles))
	args = append(args, "v", "-q")
	if opts.Turbo {
		args = append(args, "-t+")
	}
	args = append(args, parfile)
	args = append(args, extraFiles...)

	cmd := exec.CommandContext(ctx, opts.command(), args...) //nolint:gosec // parfile and extraFiles are caller-supplied, not shell-expanded
	cmd.Dir = filepath.Dir(parfile)
	cmd.Stdout = streamer
	cmd.Stderr = streamer

	runErr := cmd.Run()
	streamer.Flush()

	output := streamer.String()
	res := VerifyResult{
		Stdout: output,
		Stderr: "", // combined into Stdout now
	}
	res.Status = parseStatus(output)

	// Propagate only system-level errors, not par2's repair-required exit codes.
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return res, fmt.Errorf("par2 verify: %w", runErr)
	}

	return res, nil
}

// Repair runs `par2 r <parfile> [extraFiles...]` and attempts to repair any
// damaged files. Like Verify, it returns a RepairResult even on non-zero exit
// codes.  A non-nil error signals a system-level failure.
func Repair(ctx context.Context, parfile string, extraFiles ...string) (RepairResult, error) {
	return RepairWith(ctx, RunOptions{}, parfile, extraFiles...)
}

// RepairWith is like Repair but uses the given RunOptions for binary selection.
func RepairWith(ctx context.Context, opts RunOptions, parfile string, extraFiles ...string) (RepairResult, error) {
	streamer := cmdutil.NewLineStreamer(opts.OnLine)
	args := make([]string, 0, 3+len(extraFiles))
	args = append(args, "r", "-q")
	if opts.Turbo {
		args = append(args, "-t+")
	}
	args = append(args, parfile)
	args = append(args, extraFiles...)

	cmd := exec.CommandContext(ctx, opts.command(), args...) //nolint:gosec // parfile and extraFiles are caller-supplied, not shell-expanded
	cmd.Dir = filepath.Dir(parfile)
	cmd.Stdout = streamer
	cmd.Stderr = streamer

	runErr := cmd.Run()
	streamer.Flush()

	res := RepairResult{
		Output: streamer.String(),
	}

	var exitErr *exec.ExitError
	if runErr != nil {
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			return res, fmt.Errorf("par2 repair: %w", runErr)
		}
	}

	res.Success = strings.Contains(res.Output, "Repair complete") || strings.Contains(res.Output, "All files are correct")

	// Parse "You need N more recovery blocks" for the re-add path.
	if m := needMoreBlocksRE.FindStringSubmatch(res.Output); len(m) == 2 {
		res.NeedMoreBlocks = true
		res.BlocksNeeded, _ = strconv.Atoi(m[1])
	}

	return res, nil
}
