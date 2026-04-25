package sorting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hobeone/sabnzbd-go/internal/fsutil"
)

// SorterRule describes one user-configured sorting rule. Rules are evaluated
// in the order they appear in the slice passed to Apply; the first matching
// enabled rule wins.
type SorterRule struct {
	// Name is the display name for the rule (for logging and ApplyResult).
	Name string

	// Enabled controls whether this rule participates in matching.
	Enabled bool

	// SortString is the template string, e.g. "TV/%t/Season %0s/%t.S%0sE%0e.%ext".
	SortString string

	// Categories is the list of job categories this rule matches. An empty
	// slice means "match all categories."
	Categories []string

	// Types is the list of MediaType values this rule matches. An empty
	// slice means "match all media types."
	Types []MediaType

	// Min is the minimum job size in bytes. 0 means no minimum.
	Min int64

	// Max is the maximum job size in bytes. 0 means no maximum.
	Max int64
}

// ApplyResult reports what sorter did when Apply was called.
type ApplyResult struct {
	// MatchedRule is the Name of the SorterRule that was selected. Empty
	// when no rule matched.
	MatchedRule string

	// Moved is the list of file moves performed.
	Moved []Move
}

// Move describes a single file move performed during Apply.
type Move struct {
	From string
	To   string
}

// Apply picks the first matching rule from rules and moves files from srcDir
// into a destination path derived by ExpandTemplate. If no rule matches,
// returns an ApplyResult with MatchedRule == "" and no moves.
//
// Parameters:
//   - ctx: used for cancellation; checked between file moves.
//   - srcDir: absolute path of the completed job directory.
//   - jobCategory: the NZB's category string (compared against rule.Categories).
//   - jobName: the raw job name; used as fallback title when MediaInfo is blank.
//   - totalBytes: sum of all file sizes in the job (used for Min/Max filtering).
//   - rules: ordered list of SorterRule; first match wins.
//   - destRoot: absolute root under which the sorted sub-directory is created.
func Apply(
	ctx context.Context,
	srcDir, jobCategory, jobName string,
	totalBytes int64,
	rules []SorterRule,
	destRoot string,
) (ApplyResult, error) {
	info := Parse(jobName)
	if info.Title == "" {
		info.Title = jobName
	}

	// Find the biggest file to determine extension.
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("apply: readdir %s: %w", srcDir, err)
	}

	var filePaths []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			filePaths = append(filePaths, filepath.Join(srcDir, e.Name()))
		}
	}

	ext := biggestExt(filePaths)

	// Select the first matching rule.
	var matched *SorterRule
	for i := range rules {
		r := &rules[i]
		if !r.Enabled {
			continue
		}
		if len(r.Categories) > 0 && !containsStr(r.Categories, jobCategory) {
			continue
		}
		if len(r.Types) > 0 && !containsType(r.Types, info.Type) {
			continue
		}
		if r.Min > 0 && totalBytes < r.Min {
			continue
		}
		if r.Max > 0 && totalBytes > r.Max {
			continue
		}
		matched = r
		break
	}

	if matched == nil {
		slog.Debug("sorting: no rule matched", "job", jobName)
		return ApplyResult{}, nil
	}

	slog.Info("sorting: matched rule", "rule", matched.Name, "job", jobName)

	subpath := ExpandTemplate(matched.SortString, info, ext)
	// subpath is something like "TV/Show Name/Season 01/Show.S01E01.mkv"
	destDir := fsutil.JoinSafe(destRoot, filepath.Dir(subpath), "")

	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return ApplyResult{}, fmt.Errorf("apply: mkdir %s: %w", destDir, err)
	}

	result := ApplyResult{MatchedRule: matched.Name}

	for _, src := range filePaths {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		// If subpath looks like a full file path, use its base name.
		// Otherwise use the source file's base name.
		targetName := filepath.Base(src)
		if filepath.Base(subpath) != "." && filepath.Base(subpath) != "/" {
			targetName = filepath.Base(subpath)
		}

		dst := fsutil.JoinSafe(destDir, "", targetName)
		if moveErr := moveFile(src, dst); moveErr != nil {
			return result, fmt.Errorf("apply: move %s → %s: %w", src, dst, moveErr)
		}
		slog.Info("sorting: moved", "from", src, "to", dst)
		result.Moved = append(result.Moved, Move{From: src, To: dst})
	}

	return result, nil
}

// biggestExt returns the file extension (including leading dot) of the largest
// file in paths. Returns "" when paths is empty.
func biggestExt(paths []string) string {
	var bigSize int64
	var bigExt string
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.Size() > bigSize {
			bigSize = fi.Size()
			bigExt = filepath.Ext(p)
		}
	}
	return bigExt
}

// containsStr reports whether ss contains s (case-insensitive).
func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// containsType reports whether ts contains t.
func containsType(ts []MediaType, t MediaType) bool {
	for _, v := range ts {
		if v == t {
			return true
		}
	}
	return false
}

// moveFile moves src to dst. If os.Rename fails with a cross-device error
// (EXDEV), it falls back to copy+remove.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, crossDeviceErr()) {
		return err
	}
	// Cross-device fallback.
	return copyAndRemove(src, dst)
}

// copyAndRemove copies src to dst then removes src.
func copyAndRemove(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is a path from os.ReadDir within a known dir
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only file, close error ignorable

	out, err := os.Create(dst) //nolint:gosec // dst is constructed from destRoot + filepath.Base
	if err != nil {
		return err
	}

	if _, err = io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck // cleanup on error path
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}
