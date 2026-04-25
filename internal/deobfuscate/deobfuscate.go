// Package deobfuscate detects and renames obfuscated filenames in completed
// download directories.
//
// Scope vs. Python deobfuscate_filenames.py:
//   - TODO: extension-inference by content sniff (has_popular_extension /
//     what_is_most_likely_extension) is not implemented; only files that already
//     carry an extension are acted upon.
//   - TODO: deobfuscate_subtitles helper is not implemented.
//   - TODO: IGNORED_MOVIE_FOLDERS (DVD/Bluray) carve-out is not implemented.
//
// What IS implemented: Par2-packet-based renaming, IsProbablyObfuscated (full
// heuristic port), BiggestFile (3× size ratio guard), and Deobfuscate (rename
// biggest+siblings to usefulName).
package deobfuscate

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// excludedExts lists file extensions that are never renamed, matching Python's
// EXCLUDED_FILE_EXTS constant.
var excludedExts = map[string]bool{
	".vob":  true,
	".rar":  true,
	".par2": true,
	".mts":  true,
	".m2ts": true,
	".cpi":  true,
	".clpi": true,
	".mpl":  true,
	".mpls": true,
	".bdm":  true,
	".bdmv": true,
}

// hex32 matches a basename that is exactly 32 lowercase hex digits.
var hex32 = regexp.MustCompile(`^[a-f0-9]{32}$`)

// hex40plus matches a basename that is 40+ chars of lowercase hex + dots only.
var hex40plus = regexp.MustCompile(`^[a-f0-9.]{40,}$`)

// hex30 matches a run of 30+ consecutive lowercase hex digits anywhere in a string.
var hex30 = regexp.MustCompile(`[a-f0-9]{30}`)

// squareBracketWord matches a word wrapped in square brackets.
var squareBracketWord = regexp.MustCompile(`\[\w+\]`)

// abcXyz matches the "abc.xyz" obfuscation prefix.
var abcXyz = regexp.MustCompile(`^abc\.xyz`)

// IsProbablyObfuscated returns true if filename looks obfuscated. The
// argument may be a plain filename or a full path; only the base component
// is inspected. This is a direct port of Python's is_probably_obfuscated.
func IsProbablyObfuscated(filename string) bool {
	log := slog.Default().With("component", "deobfuscate")
	base := filepath.Base(filename)
	filebasename := strings.TrimSuffix(base, filepath.Ext(base))

	log.Debug("deobfuscate: checking", "basename", filebasename)

	// — certainly obfuscated patterns —

	// Exactly 32 lowercase hex digits.
	if hex32.MatchString(filebasename) {
		log.Debug("deobfuscate: obfuscated — 32 hex digits")
		return true
	}

	// 40+ chars of lowercase hex + dots.
	if hex40plus.MatchString(filebasename) {
		log.Debug("deobfuscate: obfuscated — 40+ hex/dot chars")
		return true
	}

	// Square-bracket tokens combined with a 30+ hex run.
	if hex30.MatchString(filebasename) && len(squareBracketWord.FindAllString(filebasename, -1)) >= 2 {
		log.Debug("deobfuscate: obfuscated — square brackets + 30-char hex")
		return true
	}

	// Starts with the literal "abc.xyz" prefix.
	if abcXyz.MatchString(filebasename) {
		log.Debug("deobfuscate: obfuscated — abc.xyz prefix")
		return true
	}

	// — signals for non-obfuscated names —

	decimals := 0
	upperchars := 0
	lowerchars := 0
	spacesdots := 0
	for _, c := range filebasename {
		switch {
		case c >= '0' && c <= '9':
			decimals++
		case c >= 'A' && c <= 'Z':
			upperchars++
		case c >= 'a' && c <= 'z':
			lowerchars++
		case c == ' ' || c == '.' || c == '_':
			spacesdots++
		}
	}

	// "Great Distro" — mixed case with at least one separator.
	if upperchars >= 2 && lowerchars >= 2 && spacesdots >= 1 {
		log.Debug("deobfuscate: not obfuscated — mixed case + separator")
		return false
	}

	// "this is a download" — three or more separators.
	if spacesdots >= 3 {
		log.Debug("deobfuscate: not obfuscated — 3+ separators")
		return false
	}

	// "Beast 2020" — letters + year-like digits + separator.
	if (upperchars+lowerchars >= 4) && decimals >= 4 && spacesdots >= 1 {
		log.Debug("deobfuscate: not obfuscated — letters+digits+sep")
		return false
	}

	// "Catullus" — starts with capital, overwhelmingly lowercase.
	if filebasename != "" && filebasename[0] >= 'A' && filebasename[0] <= 'Z' &&
		lowerchars > 2 && upperchars > 0 && float64(upperchars)/float64(lowerchars) <= 0.25 {
		log.Debug("deobfuscate: not obfuscated — capital-start mostly-lowercase")
		return false
	}

	// Short simple words (like "alpha", "multi", "test") are not obfuscated.
	if len(filebasename) >= 3 && len(filebasename) <= 10 && upperchars == 0 && decimals == 0 && spacesdots <= 1 {
		log.Debug("deobfuscate: not obfuscated — short simple word")
		return false
	}

	log.Debug("deobfuscate: obfuscated (default)")
	return true
}

// BiggestFile returns the largest file in paths (by size on disk). ok is true
// only when paths is non-empty AND the largest file is at least 3× the size
// of the second-largest. When there is exactly one file, it is returned with
// ok=true unconditionally (Python's get_biggest_file returns it as the sole
// candidate).
func BiggestFile(paths []string) (path string, ok bool, err error) {
	if len(paths) == 0 {
		return "", false, nil
	}

	type entry struct {
		path string
		size int64
	}

	entries := make([]entry, 0, len(paths))
	for _, p := range paths {
		fi, statErr := os.Stat(p)
		if statErr != nil {
			// Skip files we can't stat; propagate unexpected errors.
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return "", false, fmt.Errorf("stat %s: %w", p, statErr)
		}
		entries = append(entries, entry{p, fi.Size()})
	}

	if len(entries) == 0 {
		return "", false, nil
	}

	// Find biggest and second-biggest without sorting.
	biggest := entries[0]
	var second entry
	for _, e := range entries[1:] {
		if e.size > biggest.size {
			second = biggest
			biggest = e
		} else if e.size > second.size {
			second = e
		}
	}

	if len(entries) == 1 {
		return biggest.path, true, nil
	}

	if second.size == 0 {
		// Avoid division by zero; treat as "clearly biggest".
		return biggest.path, true, nil
	}

	factor := float64(biggest.size) / float64(second.size)
	if factor > 3.0 {
		return biggest.path, true, nil
	}
	return "", false, nil
}

// Rename describes a single file rename performed by Deobfuscate.
type Rename struct {
	From string
	To   string
}

// Deobfuscate scans dir for obfuscated files. It first attempts to use PAR2
// metadata for renaming. If no PAR2 files are present or no renames occur,
// it falls back to the "biggest file" heuristic and renames it (and any
// same-stem siblings) to usefulName + original extension. Returns the list
// of renames actually performed. Returns nil, nil when no rename is needed.
func Deobfuscate(dir, usefulName string) ([]Rename, error) {
	log := slog.Default().With("component", "deobfuscate")

	// 1. Attempt PAR2-based deobfuscation first.
	parRenames, err := Par2Rename(dir)
	if err != nil {
		log.Warn("deobfuscate: par2 deobfuscation encountered an error", "dir", dir, "err", err)
	}
	if len(parRenames) > 0 {
		log.Debug("deobfuscate: par2-based renaming successful — skipping heuristic")
		return parRenames, nil
	}

	// 2. Fall back to heuristic: find the qualifying biggest file.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("readdir %s: %w", dir, err)
	}

	var paths []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}

	bigPath, ok, err := BiggestFile(paths)
	if err != nil {
		return nil, err
	}
	if !ok {
		log.Debug("deobfuscate: no qualifying biggest file found", "dir", dir)
		return nil, nil
	}

	ext := strings.ToLower(filepath.Ext(bigPath))
	if excludedExts[ext] {
		log.Debug("deobfuscate: biggest file has excluded extension — skipping", "path", bigPath, "ext", ext)
		return nil, nil
	}

	if !IsProbablyObfuscated(bigPath) {
		log.Debug("deobfuscate: biggest file not obfuscated — skipping", "path", bigPath)
		return nil, nil
	}

	var renames []Rename

	// Rename the biggest file.
	newBigPath, err := uniqueName(filepath.Join(dir, usefulName+filepath.Ext(bigPath)))
	if err != nil {
		return nil, err
	}
	if err := os.Rename(bigPath, newBigPath); err != nil {
		return nil, fmt.Errorf("rename %s → %s: %w", bigPath, newBigPath, err)
	}
	log.Info("deobfuscate: renamed", "from", bigPath, "to", newBigPath)
	renames = append(renames, Rename{From: bigPath, To: newBigPath})

	// basedirfile is the path without extension — used to find siblings.
	baseDirFile := strings.TrimSuffix(bigPath, filepath.Ext(bigPath))

	// Rename siblings that share the same stem (e.g. "file-sample.iso").
	for _, p := range paths {
		if p == bigPath {
			continue
		}
		if !strings.HasPrefix(p, baseDirFile) {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		remainingSuffix := strings.TrimPrefix(p, baseDirFile)
		newPath, nameErr := uniqueName(filepath.Join(dir, usefulName+remainingSuffix))
		if nameErr != nil {
			return renames, nameErr
		}
		if renErr := os.Rename(p, newPath); renErr != nil {
			return renames, fmt.Errorf("rename sibling %s → %s: %w", p, newPath, renErr)
		}
		log.Info("deobfuscate: renamed sibling", "from", p, "to", newPath)
		renames = append(renames, Rename{From: p, To: newPath})
	}

	return renames, nil
}

// uniqueName returns candidate if it does not exist, otherwise appends an
// incrementing counter until it finds a free name.
func uniqueName(candidate string) (string, error) {
	if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
		return candidate, nil
	}
	ext := filepath.Ext(candidate)
	base := strings.TrimSuffix(candidate, ext)
	for i := 1; i < 10000; i++ {
		name := fmt.Sprintf("%s.%d%s", base, i, ext)
		if _, err := os.Stat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		}
	}
	return "", fmt.Errorf("could not find unique name for %s", candidate)
}
