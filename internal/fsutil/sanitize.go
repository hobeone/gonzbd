package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
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
	maxFilenameBytes = 245
	maxExtensionLen  = 20
	maxPathBytes     = 250 // Safe limit for Windows (260) and others
)

// SanitizeOptions defines how to handle illegal characters and spaces.
type SanitizeOptions struct {
	ReplaceIllegalWith string
	ReplaceSpacesWith  string
	StripDiacritics    bool
}

// JoinSafe joins a base directory, folder name, and filename into a single
// absolute path, ensuring that the result does not exceed maxPathBytes.
// If the path is too long, it truncates the folder name first, then the
// filename if necessary.
func JoinSafe(base, folder, file string, opts SanitizeOptions) string {
	// 1. Sanitize the folder and file components first.
	// Base is assumed to be a trusted absolute path from the caller.
	if folder != "" {
		folder = SanitizeFolderName(folder, opts)
	}
	if file != "" {
		file = SanitizeFilename(file, opts)
	}

	// 2. Initial path.
	var fullPath string
	switch {
	case folder != "" && file != "":
		fullPath = filepath.Join(base, folder, file)
	case folder != "":
		fullPath = filepath.Join(base, folder)
	case file != "":
		fullPath = filepath.Join(base, file)
	default:
		return base
	}

	if len(fullPath) <= maxPathBytes {
		return fullPath
	}

	// 3. We are over the limit. Try truncating the folder name first.
	if folder != "" {
		// Calculate space remaining for folder.
		var overhead int
		if file != "" {
			overhead = len(filepath.Join(base, "", file)) + 1
		} else {
			overhead = len(base) + 1
		}
		maxFolderLen := maxPathBytes - overhead

		if maxFolderLen >= 10 {
			folder = truncateFilename(folder, maxFolderLen)
			if file != "" {
				fullPath = filepath.Join(base, folder, file)
			} else {
				fullPath = filepath.Join(base, folder)
			}
			if len(fullPath) <= maxPathBytes {
				return fullPath
			}
		}
	}

	// 4. Folder truncation wasn't enough, or folder is empty/already tiny.
	// Truncate the file name to fit in the remaining space.
	if file != "" {
		var currentBaseAndFolder string
		if folder != "" {
			currentBaseAndFolder = filepath.Join(base, folder)
		} else {
			currentBaseAndFolder = base
		}

		maxFileLen := maxPathBytes - len(currentBaseAndFolder) - 1
		if maxFileLen < 5 {
			// Path is extremely constrained. Hard truncate to survive.
			return fullPath[:maxPathBytes]
		}

		file = truncateFilename(file, maxFileLen)
		return filepath.Join(currentBaseAndFolder, file)
	}

	return fullPath[:maxPathBytes]
}

// SanitizeFilename cleans up a filename to ensure it is safe for all filesystems.
// It follows the logic of Python SABnzbd's sanitize_filename.
func SanitizeFilename(filename string, opts SanitizeOptions) string {
	if filename == "" {
		return "unknown"
	}

	// 1. NFC Normalization (standard first step)
	filename = norm.NFC.String(filename)

	// 2. Strip diacritics if requested
	if opts.StripDiacritics {
		filename = stripDiacritics(filename)
	}

	// 3. Remove illegal characters (control characters 0-31 and Windows reserved characters)
	illegal := "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f" +
		"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f" +
		`\/:*?"<>|`

	illegalReplacement := opts.ReplaceIllegalWith
	if illegalReplacement == "" {
		illegalReplacement = "_"
	}

	for _, char := range illegal {
		filename = strings.ReplaceAll(filename, string(char), illegalReplacement)
	}

	if opts.ReplaceSpacesWith != "" {
		filename = strings.ReplaceAll(filename, " ", opts.ReplaceSpacesWith)
	}

	filename = strings.TrimSpace(filename)

	// 4. Replace Windows reserved device names
	filename = replaceWinDevices(filename)

	// 5. Remove trailing dots and spaces (invalid on Windows)
	for len(filename) > 0 && (filename[len(filename)-1] == '.' || filename[len(filename)-1] == ' ') {
		filename = strings.TrimRight(filename, ". ")
	}

	if filename == "" {
		return "unknown"
	}

	// 6. Truncate length while preserving extension.
	return truncateFilename(filename, maxFilenameBytes)
}

// SanitizeFolderName cleans up a folder name to ensure it is safe for all filesystems.
// It follows the logic of Python SABnzbd's sanitize_foldername.
func SanitizeFolderName(foldername string, opts SanitizeOptions) string {
	if foldername == "" {
		return "unknown"
	}

	// 1. NFC Normalization
	foldername = norm.NFC.String(foldername)

	// 2. Strip diacritics if requested
	if opts.StripDiacritics {
		foldername = stripDiacritics(foldername)
	}

	// 3. Remove illegal characters
	illegal := "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f" +
		"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f" +
		`\/:*?"<>|`

	illegalReplacement := opts.ReplaceIllegalWith
	if illegalReplacement == "" {
		illegalReplacement = "_"
	}

	for _, char := range illegal {
		foldername = strings.ReplaceAll(foldername, string(char), illegalReplacement)
	}

	if opts.ReplaceSpacesWith != "" {
		foldername = strings.ReplaceAll(foldername, " ", opts.ReplaceSpacesWith)
	}

	foldername = strings.TrimSpace(foldername)

	// 4. Replace Windows reserved device names
	foldername = replaceWinDevices(foldername)

	// 5. Truncate length
	if len(foldername) > maxFilenameBytes {
		foldername = truncateFilename(foldername, maxFilenameBytes)
	}

	// 6. Remove trailing dots and spaces (invalid on Windows)
	for len(foldername) > 0 && (foldername[len(foldername)-1] == '.' || foldername[len(foldername)-1] == ' ') {
		foldername = strings.TrimRight(foldername, ". ")
	}

	if foldername == "" {
		return "unknown"
	}

	return foldername
}

// stripDiacritics replaces accented characters with their ASCII equivalents.
func stripDiacritics(s string) string {
	// 1. Decompose into NFD (e.g. é -> e + ´)
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	result, _, _ := transform.String(t, s) //nolint:errcheck // best-effort stripping
	return result
}

// IsObfuscated returns true if the filename looks like an obfuscated hash
// (e.g., 32, 40, or 64 hex characters).
func IsObfuscated(name string) bool {
	name = strings.TrimSuffix(name, filepath.Ext(name))
	l := len(name)
	if l != 32 && l != 40 && l != 64 && l != 128 {
		return false
	}
	for _, r := range name {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
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

// GetUniqueFilename returns a unique version of the path by appending .1, .2, etc.
// if the file already exists on disk.
func GetUniqueFilename(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}

	ext := filepath.Ext(path)
	base := path[:len(path)-len(ext)]
	for i := 1; ; i++ {
		newPath := fmt.Sprintf("%s.%d%s", base, i, ext)
		if _, err := os.Stat(newPath); os.IsNotExist(err) {
			return newPath
		}
	}
}
