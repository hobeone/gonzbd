// Package directunpack implements streaming RAR extraction during download.
//
// When enabled, completed RAR volumes are fed to an interactive `unrar -vp`
// subprocess as they arrive. The subprocess pauses at each volume boundary
// and waits for the next volume to be signaled. This overlaps download I/O
// with extraction I/O, reducing total pipeline wall-clock time.
//
// DirectUnpack is purely additive — it never modifies the existing unpack
// stage. On any error the subprocess is killed, partial extracts are cleaned
// up, and the standard post-processing unpack stage runs as if DirectUnpack
// never existed.
package directunpack

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// partPattern matches new-style multi-part RAR names: movie.part01.rar.
var partPattern = regexp.MustCompile(`(?i)(.+)\.part(\d+)\.rar$`)

// legacyMainPattern matches the main volume: movie.rar (no .partNN).
var legacyMainPattern = regexp.MustCompile(`(?i)(.+)\.rar$`)

// legacyExtraPattern matches legacy extra volumes: movie.r00, movie.r01, …
var legacyExtraPattern = regexp.MustCompile(`(?i)(.+)\.r(\d+)$`)

// AnalyzeRarFilename parses a RAR volume filename into its set name and
// 1-based volume number. Only the base filename is considered (directory
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
	if m := partPattern.FindStringSubmatch(base); len(m) == 3 {
		vol, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0
		}
		return strings.ToLower(m[1]), vol
	}

	// Legacy extra volumes must be checked BEFORE legacy main,
	// because .r00 would also match .rar$ via substring.

	// Legacy extra: movie.r00 = vol 2, movie.r01 = vol 3, etc.
	if m := legacyExtraPattern.FindStringSubmatch(base); len(m) == 3 {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0
		}
		return strings.ToLower(m[1]), n + 2
	}

	// Legacy main: movie.rar = vol 1
	if m := legacyMainPattern.FindStringSubmatch(base); len(m) == 2 {
		// Exclude files that matched partPattern (already handled above)
		// and files that are actually .rNN (should not reach here due to
		// ordering, but guard defensively).
		return strings.ToLower(m[1]), 1
	}

	return "", 0
}
