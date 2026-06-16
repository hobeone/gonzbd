// Package fsutil provides filesystem utilities: path sanitization,
// cross-device file moves, and obfuscation detection.
package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var reservedNames = []string{
	"CON", "PRN", "AUX", "NUL",
	"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
	"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
}

const (
	// maxFilenameBytes is the per-component limit. NAME_MAX is 255 bytes on
	// ext4/xfs/btrfs/zfs; exceeding it causes ENAMETOOLONG. We don't enforce
	// a total-path budget — modern Linux has no hard PATH_MAX in the VFS.
	maxFilenameBytes = 255
	maxExtensionLen  = 20
)

// SanitizeOptions defines how to handle illegal characters and spaces.
type SanitizeOptions struct {
	ReplaceIllegalWith string
	ReplaceSpacesWith  string
	StripDiacritics    bool
	CleanupList        []string
	// CleanupRegexps holds pre-compiled versions of CleanupList patterns.
	// When non-nil, CleanupName uses these instead of re-compiling on every
	// call. Use CompileCleanupList to populate this from CleanupList.
	CleanupRegexps []*regexp.Regexp
}

// JoinSafe joins a base directory, folder name, and filename into a single
// absolute path. Each component is sanitized and capped at maxFilenameBytes
// (the per-component NAME_MAX limit). No total-path budget is enforced —
// folder names are stable across calls regardless of filename length, so all
// files in a job land in the same directory.
func JoinSafe(base, folder, file string, opts SanitizeOptions) string {
	// Base is assumed to be a trusted absolute path from the caller.
	if folder != "" {
		folder = SanitizeFolderName(folder, opts)
	}
	if file != "" {
		file = SanitizeFilename(file, opts)
	}

	switch {
	case folder != "" && file != "":
		return filepath.Join(base, folder, file)
	case folder != "":
		return filepath.Join(base, folder)
	case file != "":
		return filepath.Join(base, file)
	default:
		return base
	}
}

// SanitizeFilename cleans a filename so it is safe for all filesystems:
// illegal characters replaced, Windows reserved names defused, trailing
// dots/spaces stripped, and the result truncated to NAME_MAX bytes
// preserving the extension. Follows SABnzbd's sanitize_filename.
func SanitizeFilename(filename string, opts SanitizeOptions) string {
	if filename == "" {
		return "unknown"
	}
	filename = sanitizeCore(filename, opts)
	filename = trimTrailingDotAndSpace(filename)
	filename = truncateFilename(filename, maxFilenameBytes)
	// Truncation can re-expose a trailing dot/space that the cut landed
	// on; trim again so the result is stable (idempotent).
	filename = trimTrailingDotAndSpace(filename)
	if filename == "" {
		return "unknown"
	}
	return filename
}

// SanitizeFolderName cleans a folder name. Unlike filenames, truncation
// happens before trailing-dot stripping so a truncated folder never ends
// in a dot. Follows SABnzbd's sanitize_foldername.
func SanitizeFolderName(foldername string, opts SanitizeOptions) string {
	if foldername == "" {
		return "unknown"
	}
	foldername = sanitizeCore(foldername, opts)
	if len(foldername) > maxFilenameBytes {
		foldername = truncateFilename(foldername, maxFilenameBytes)
	}
	foldername = trimTrailingDotAndSpace(foldername)
	if foldername == "" {
		return "unknown"
	}
	return foldername
}

// sanitizeCore applies the shared sanitization steps used by both
// SanitizeFilename and SanitizeFolderName: NFC normalization, optional
// diacritics removal, illegal character replacement, space replacement,
// trimming, and Windows reserved-name handling. The caller is responsible
// for truncation and trailing-dot policy, which differ between files and
// folders.
func sanitizeCore(name string, opts SanitizeOptions) string {
	// 1. NFC Normalization (standard first step).
	name = norm.NFC.String(name)

	// 2. Strip diacritics if requested.
	if opts.StripDiacritics {
		name = stripDiacritics(name)
	}

	// 3. Remove illegal characters (control characters 0-31 and Windows reserved characters).
	illegal := "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f" +
		"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f" +
		`\/:*?"<>|`
	illegalReplacement := opts.ReplaceIllegalWith
	if illegalReplacement == "" {
		illegalReplacement = "_"
	}
	for _, char := range illegal {
		name = strings.ReplaceAll(name, string(char), illegalReplacement)
	}

	// 4. Replace spaces if configured.
	if opts.ReplaceSpacesWith != "" {
		name = strings.ReplaceAll(name, " ", opts.ReplaceSpacesWith)
	}

	name = strings.TrimSpace(name)

	// 5. Replace Windows reserved device names.
	name = replaceWinDevices(name)

	return name
}

// stripDiacritics replaces accented characters with their ASCII equivalents.
func stripDiacritics(s string) string {
	// 1. Decompose into NFD (e.g. é -> e + ´)
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	result, _, _ := transform.String(t, s) //nolint:errcheck // best-effort stripping
	return result
}

// CleanupName removes common spammy prefixes and suffixes from a job name.
// It uses pre-compiled regexes from opts.CleanupRegexps when available,
// falling back to compiling from opts.CleanupList on each call.
func CleanupName(name string, opts SanitizeOptions) string {
	regexps := opts.CleanupRegexps
	if regexps == nil {
		// Fallback: compile on each call (legacy callers, tests).
		regexps = CompileCleanupList(opts.CleanupList)
	}
	for _, re := range regexps {
		name = re.ReplaceAllString(name, "")
	}
	return strings.TrimSpace(name)
}

// CompileCleanupList pre-compiles a list of regex pattern strings.
// Invalid patterns are silently skipped (config validation catches them).
func CompileCleanupList(patterns []string) []*regexp.Regexp {
	var compiled []*regexp.Regexp
	for _, pat := range patterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			continue
		}
		compiled = append(compiled, re)
	}
	return compiled
}

func replaceWinDevices(name string) string {
	lower := strings.ToLower(name)
	for _, res := range reservedNames {
		resLower := strings.ToLower(res)
		if lower == resLower || strings.HasPrefix(lower, resLower+".") {
			return "_" + name
		}
	}
	// Special NTFS filename
	if strings.HasPrefix(lower, "$mft") {
		return "S" + name[1:]
	}
	return name
}

// truncateFilename caps filename at maxBytes, preserving the extension
// (itself capped at maxExtensionLen) and never splitting a multi-byte
// UTF-8 rune.
func truncateFilename(filename string, maxBytes int) string {
	if len(filename) <= maxBytes {
		return filename
	}

	ext := truncateOnRuneBoundary(filepath.Ext(filename), maxExtensionLen)
	base := filename[:len(filename)-len(filepath.Ext(filename))]
	maxBaseBytes := maxBytes - len(ext)
	if maxBaseBytes <= 0 {
		return truncateOnRuneBoundary(filename, maxBytes)
	}
	return truncateOnRuneBoundary(base, maxBaseBytes) + ext
}

// truncateOnRuneBoundary cuts s to at most maxBytes without splitting a
// multi-byte UTF-8 rune (the cut backs off to the nearest rune start).
func truncateOnRuneBoundary(s string, maxBytes int) string {
	if maxBytes < 1 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func trimTrailingDotAndSpace(s string) string {
	return strings.TrimRightFunc(s, func(r rune) bool {
		return r == '.' || unicode.IsSpace(r)
	})
}

var maxAttempts = 10_000

// GetUniqueFilename returns a unique version of the path by appending .1, .2, etc.
// if the file already exists on disk. Stops after 10,000 attempts to prevent
// unbounded iteration on pathologically crowded directories.
func GetUniqueFilename(path string) string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}

	ext := filepath.Ext(path)
	base := path[:len(path)-len(ext)]
	for i := 1; i <= maxAttempts; i++ {
		newPath := fmt.Sprintf("%s.%d%s", base, i, ext)
		if _, err := os.Stat(newPath); errors.Is(err, os.ErrNotExist) {
			return newPath
		}
	}
	// Exhausted attempts; return the last candidate (extremely unlikely).
	return fmt.Sprintf("%s.%d%s", base, maxAttempts, ext)
}
