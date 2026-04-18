package fsutil

import (
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var reservedNames = []string{
	"CON", "PRN", "AUX", "NUL",
	"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
	"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
}

const (
	maxFilenameBytes = 245
	maxExtensionLen  = 20
)

// SanitizeFilename cleans up a filename to ensure it is safe for all filesystems.
// It follows the logic of Python SABnzbd's sanitize_filename.
func SanitizeFilename(filename string) string {
	if filename == "" {
		return "unknown"
	}

	// 1. NFC Normalization
	filename = norm.NFC.String(filename)

	// 2. Remove illegal characters (control characters 0-31 and Windows reserved characters)
	// We are strict by default to ensure portability.
	illegal := "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f" +
		"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f" +
		`\/:*?"<>|`

	filename = strings.Map(func(r rune) rune {
		if strings.ContainsRune(illegal, r) {
			return '_'
		}
		return r
	}, filename)

	filename = strings.TrimSpace(filename)

	// 3. Replace Windows reserved device names
	filename = replaceWinDevices(filename)

	if filename == "" {
		return "unknown"
	}

	// 4. Truncate length while preserving extension.
	return truncateFilename(filename, maxFilenameBytes)
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

func truncateFilename(filename string, maxBytes int) string {
	if len(filename) <= maxBytes {
		return filename
	}

	ext := filepath.Ext(filename)
	// If extension itself is somehow huge, truncate it too (rare but safe)
	if len(ext) > maxExtensionLen {
		ext = ext[:maxExtensionLen]
	}

	base := filename[:len(filename)-len(filepath.Ext(filename))]
	maxBaseBytes := maxBytes - len(ext)

	if maxBaseBytes <= 0 {
		// Extremely rare case: extension is longer than maxBytes
		// Just hard truncate
		return filename[:maxBytes]
	}

	// Truncate base to maxBaseBytes, ensuring we don't break a multi-byte UTF-8 character.
	truncatedBase := ""
	currentBytes := 0
	for _, r := range base {
		rLen := utf8.RuneLen(r)
		if currentBytes+rLen > maxBaseBytes {
			break
		}
		truncatedBase += string(r)
		currentBytes += rLen
	}

	return truncatedBase + ext
}
