package rarheader

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// rarPartPattern matches new-style multi-part RAR names: movie.part01.rar.
var rarPartPattern = regexp.MustCompile(`(?i)(.+)\.part(\d+)\.rar$`)

// rarMainPattern matches the legacy main volume: movie.rar (no .partNN).
var rarMainPattern = regexp.MustCompile(`(?i)(.+)\.rar$`)

// rarExtraPattern matches legacy extra volumes: movie.r00, movie.r01, …
var rarExtraPattern = regexp.MustCompile(`(?i)(.+)\.r(\d+)$`)

// AnalyzeRarFilename parses a RAR volume filename (or subject) into its set name
// and 1-based volume number. Only the base filename is considered (directory
// components are stripped).
//
// Returns ("", 0) for non-RAR files.
//
// Naming conventions:
//
//	movie.part01.rar  → ("movie", 1)
//	movie.part02.rar  → ("movie", 2)
//	movie.rar         → ("movie", 1)        // legacy main volume
//	movie.r00         → ("movie", 2)        // legacy: r00 = volume 2
//	movie.r01         → ("movie", 3)        // legacy: r01 = volume 3
func AnalyzeRarFilename(filename string) (setname string, volume int) {
	base := filepath.Base(filename)

	// New-style: movie.part01.rar
	if m := rarPartPattern.FindStringSubmatch(base); len(m) == 3 {
		vol, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0
		}
		return strings.ToLower(m[1]), vol
	}

	// Legacy extra volumes must be checked BEFORE legacy main,
	// because .r00 would also match .rar$ via substring.

	// Legacy extra: movie.r00 = vol 2, movie.r01 = vol 3, etc.
	if m := rarExtraPattern.FindStringSubmatch(base); len(m) == 3 {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0
		}
		return strings.ToLower(m[1]), n + 2
	}

	// Legacy main: movie.rar = vol 1
	if m := rarMainPattern.FindStringSubmatch(base); len(m) == 2 {
		return strings.ToLower(m[1]), 1
	}

	return "", 0
}
