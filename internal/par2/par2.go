// Package par2 provides functions to find, verify, and repair par2 sets by
// shelling out to the par2 command-line binary.
package par2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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
	// StatusInvalidPar2 means the par2 file itself is missing or corrupt.
	StatusInvalidPar2
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
}

// volPattern matches the volume suffix of a par2 filename, e.g. ".vol000+01".
var volPattern = regexp.MustCompile(`(?i)\.vol\d+\+\d+$`)

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
	entries, err := filepath.Glob(filepath.Join(dir, "*.par2"))
	if err != nil {
		return nil, fmt.Errorf("par2 find: %w", err)
	}

	// Also catch upper-case variants (.PAR2).
	upper, err := filepath.Glob(filepath.Join(dir, "*.PAR2"))
	if err != nil {
		return nil, fmt.Errorf("par2 find: %w", err)
	}
	entries = append(entries, upper...)

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

	// Stable sort by name for deterministic output.
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j].Name < result[j-1].Name; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}

	return result, nil
}

// parseStatus inspects par2 output lines and returns the most specific Status.
func parseStatus(output string) Status {
	switch {
	case strings.Contains(output, "All files are correct"):
		return StatusAllFilesOK
	case strings.Contains(output, "Repair is not possible"):
		return StatusRepairNotPossible
	case strings.Contains(output, "Repair is possible"):
		return StatusRepairPossible
	case strings.Contains(output, "Repair is required"):
		return StatusRepairRequired
	case strings.Contains(output, "Main packet not found"),
		strings.Contains(output, "The recovery file does not exist"):
		return StatusInvalidPar2
	default:
		return StatusUnknown
	}
}

// Verify runs `par2 r <parfile> [extraFiles...]` and parses the output to
// determine whether the protected files are intact.  It returns a VerifyResult
// even when par2 itself exits with a non-zero code, as long as the process
// could be started. A returned error indicates a system-level failure (binary
// not found, context cancelled, etc.).
func Verify(ctx context.Context, parfile string, extraFiles ...string) (VerifyResult, error) {
	var stdout, stderr bytes.Buffer
	args := make([]string, 0, 2+len(extraFiles))
	args = append(args, "r", parfile)
	args = append(args, extraFiles...)

	cmd := exec.CommandContext(ctx, "par2", args...) //nolint:gosec // parfile and extraFiles are caller-supplied, not shell-expanded
	cmd.Dir = filepath.Dir(parfile)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	res := VerifyResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	combined := res.Stdout + res.Stderr
	res.Status = parseStatus(combined)

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
	var combined bytes.Buffer
	args := make([]string, 0, 2+len(extraFiles))
	args = append(args, "r", parfile)
	args = append(args, extraFiles...)

	cmd := exec.CommandContext(ctx, "par2", args...) //nolint:gosec // parfile and extraFiles are caller-supplied, not shell-expanded
	cmd.Dir = filepath.Dir(parfile)
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	runErr := cmd.Run()

	res := RepairResult{
		Output: combined.String(),
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
	return res, nil
}
