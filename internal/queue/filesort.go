package queue

import (
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// rarPartPattern matches new-style multi-part RAR names: movie.part01.rar.
var rarPartPattern = regexp.MustCompile(`(?i)(.+)\.part(\d+)\.rar$`)

// rarMainPattern matches the legacy main volume: movie.rar (no .partNN).
var rarMainPattern = regexp.MustCompile(`(?i)(.+)\.rar$`)

// rarExtraPattern matches legacy extra volumes: movie.r00, movie.r01, …
var rarExtraPattern = regexp.MustCompile(`(?i)(.+)\.r(\d+)$`)

// analyzeRarSubject parses a file subject (or filename) into its RAR set name
// and 1-based volume number. Returns ("", 0) for non-RAR files.
// Mirrors directunpack.AnalyzeRarFilename — kept separate to avoid a
// circular import between queue and directunpack.
func analyzeRarSubject(subject string) (setname string, volume int) {
	base := filepath.Base(subject)

	if m := rarPartPattern.FindStringSubmatch(base); len(m) == 3 {
		vol, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0
		}
		return strings.ToLower(m[1]), vol
	}

	// Legacy extra must be checked before legacy main (.r00 would also match .rar$).
	if m := rarExtraPattern.FindStringSubmatch(base); len(m) == 3 {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0
		}
		return strings.ToLower(m[1]), n + 2
	}

	if m := rarMainPattern.FindStringSubmatch(base); len(m) == 2 {
		return strings.ToLower(m[1]), 1
	}

	return "", 0
}

// rarSortKey returns the sort key for a JobFile.
// RAR volumes sort first (tier 0), ordered by (setname, volume).
// All other files keep their relative document order (tier 1).
type rarSortKey struct {
	tier    int
	setname string
	vol     int
}

func fileRarSortKey(f JobFile) rarSortKey {
	setname, vol := analyzeRarSubject(f.Subject)
	if setname != "" {
		return rarSortKey{tier: 0, setname: setname, vol: vol}
	}
	return rarSortKey{tier: 1}
}

// sortJobFiles reorders files so that RAR volumes appear first, sorted by
// (set name, volume number). Non-RAR files retain their relative document
// order. The sort is stable.
func sortJobFiles(files []JobFile) {
	slices.SortStableFunc(files, func(a, b JobFile) int {
		ka, kb := fileRarSortKey(a), fileRarSortKey(b)
		if ka.tier != kb.tier {
			return ka.tier - kb.tier
		}
		if ka.tier == 0 {
			if ka.setname != kb.setname {
				return strings.Compare(ka.setname, kb.setname)
			}
			return ka.vol - kb.vol
		}
		return 0 // both non-RAR: stable preserves document order
	})
}
