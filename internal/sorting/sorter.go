package sorting

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hobeone/gonzbd/internal/fsutil"
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
	opts fsutil.SanitizeOptions,
) (ApplyResult, error) {
	info := Parse(jobName)
	if info.Title == "" {
		info.Title = jobName
	}

	// Recursively collect all regular files — archives may extract into
	// subdirectories, and we must move everything to prevent data loss
	// when origDir is cleaned up after sorting.
	var filePaths []string
	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			filePaths = append(filePaths, path)
		}
		return nil
	})
	if err != nil {
		return ApplyResult{}, fmt.Errorf("apply: walk %s: %w", srcDir, err)
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
	// subpath is something like "TV/%t/Season %0s" -> "TV/Show Name/Season 01"
	// We must join each component separately so JoinSafe doesn't underscores the slashes.
	parts := strings.Split(filepath.ToSlash(subpath), "/")

	// Determine if the sort string specifies a filename (not just a directory).
	// When it does, only the biggest file gets the template-derived name;
	// all other files keep their original names to prevent overwrites.
	// We check this early so we can exclude the filename part from the
	// directory construction below.
	templateSpecifiesFile := !strings.HasSuffix(matched.SortString, "/") &&
		filepath.Ext(subpath) != ""

	// When the template specifies a filename, the last part of subpath IS
	// the desired filename, not a directory. Exclude it from MkdirAll to
	// avoid creating a directory named after the file (e.g. "Show.S01E01.mkv/").
	dirParts := parts
	if templateSpecifiesFile && len(parts) > 0 {
		dirParts = parts[:len(parts)-1]
	}

	destDir := destRoot
	for _, p := range dirParts {
		if p == "" || p == "." {
			continue
		}
		destDir = fsutil.JoinSafe(destDir, p, "", opts)
	}

	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return ApplyResult{}, fmt.Errorf("apply: mkdir %s: %w", destDir, err)
	}

	result := ApplyResult{MatchedRule: matched.Name}

	// Find the biggest file path so we know which one gets the template name.
	var biggestFile string
	if templateSpecifiesFile {
		biggestSize := int64(-1)
		for _, p := range filePaths {
			if fi, err := os.Stat(p); err == nil && fi.Size() > biggestSize {
				biggestSize = fi.Size()
				biggestFile = p
			}
		}
	}

	for _, src := range filePaths {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		targetName := filepath.Base(src)
		if templateSpecifiesFile && src == biggestFile {
			// The biggest file gets the template-derived name (with correct ext).
			targetName = filepath.Base(subpath)
		}

		// For files in subdirectories, preserve relative structure
		// under destDir to avoid name collisions.
		relDir := ""
		if rel, err := filepath.Rel(srcDir, filepath.Dir(src)); err == nil && rel != "." {
			relDir = rel
		}

		moveDir := destDir
		if relDir != "" {
			moveDir = filepath.Join(destDir, relDir)
			if mkErr := os.MkdirAll(moveDir, 0o750); mkErr != nil {
				return result, fmt.Errorf("apply: mkdir %s: %w", moveDir, mkErr)
			}
		}

		dst := fsutil.JoinSafe(moveDir, "", targetName, opts)
		dst = fsutil.GetUniqueFilename(dst)

		if moveErr := fsutil.MoveFile(src, dst); moveErr != nil {
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
	bigSize := int64(-1)
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
