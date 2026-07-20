package unpack

import "strings"

// FailReason classifies why an extraction failed, enabling appropriate user
// feedback and recovery logic.
type FailReason int

const (
	// FailUnknown is the default: the extraction failed for an unspecified reason.
	FailUnknown FailReason = iota
	// FailWrongPassword means the archive requires a different password.
	FailWrongPassword
	// FailDiskFull means the output device has insufficient space.
	FailDiskFull
	// FailCorrupt means the archive data is corrupted (CRC error, bad header, etc.).
	FailCorrupt
	// FailMissingVolume means a volume in a multi-part set is missing.
	FailMissingVolume
	// FailNotArchive means the file is not a valid archive format.
	FailNotArchive
	// FailFileTooLarge means a file in the archive exceeds the
	// filesystem's or OS's maximum file size.
	FailFileTooLarge
)

// String returns a human-readable label for the FailReason.
func (r FailReason) String() string {
	switch r {
	case FailWrongPassword:
		return "wrong password"
	case FailDiskFull:
		return "disk full"
	case FailCorrupt:
		return "corrupt archive"
	case FailMissingVolume:
		return "missing volume"
	case FailNotArchive:
		return "not an archive"
	case FailFileTooLarge:
		return "file too large"
	default:
		return "unknown error"
	}
}

// ClassifyUnrarOutput examines unrar's combined stdout+stderr output and
// returns a FailReason. This matches SABnzbd's unrar output parsing from
// newsunpack.py (lines 730–860).
//
// isUnrarPasswordError checks lowercased unrar output for wrong password indicators.
func isUnrarPasswordError(lower string) bool {
	return strings.Contains(lower, "incorrect password") ||
		strings.Contains(lower, "the specified password is incorrect") ||
		strings.Contains(lower, "checksum error in the encrypted file") ||
		// Unrar variants: "Encrypted file:  CRC failed in ..." (variable spacing).
		(strings.Contains(lower, "encrypted file") && strings.Contains(lower, "crc failed"))
}

// ClassifyUnrarOutput inspects unrar command output and maps it to a FailReason.
// Only call this when the extraction has already failed (exit code != 0).
func ClassifyUnrarOutput(output string) FailReason {
	lower := strings.ToLower(output)

	// Wrong password patterns (most specific — check first).
	if isUnrarPasswordError(lower) {
		return FailWrongPassword
	}

	// Disk full.
	if strings.Contains(lower, "write error") ||
		strings.Contains(lower, "there is not enough space on the disk") ||
		strings.Contains(lower, "no space left on device") {
		return FailDiskFull
	}

	// Not a RAR archive.
	if strings.Contains(lower, "is not rar archive") || strings.Contains(lower, "bad archive") {
		return FailNotArchive
	}

	// Missing volume (but not during recovery mode).
	// N3: When unrar is reconstructing missing volumes from .rev files,
	// it prints "Cannot find volume" followed by "Reconstructing" or
	// "Recovery volumes found". Suppress the error in these cases.
	// Matches SABnzbd's inrecovery flag behavior.
	if strings.Contains(lower, "cannot find volume") &&
		!strings.Contains(lower, "recovery volumes found") &&
		!strings.Contains(lower, "reconstructing") {
		return FailMissingVolume
	}

	// CRC / corrupt.
	if strings.Contains(lower, "checksum error") ||
		strings.Contains(lower, "unexpected end of archive") ||
		strings.Contains(lower, "- crc failed") ||
		strings.Contains(lower, "corrupt header") ||
		strings.Contains(lower, "header is corrupt") {
		return FailCorrupt
	}

	// File too large for the filesystem (e.g. >4 GiB on FAT32).
	if strings.Contains(lower, "file too large") {
		return FailFileTooLarge
	}

	return FailUnknown
}

// Classify7zOutput examines 7z's combined stdout+stderr output and returns
// a FailReason. Matches SABnzbd's seven_extract_core parsing.
//
// Only call this when the extraction has already failed (exit code != 0).
func Classify7zOutput(output string) FailReason {
	lower := strings.ToLower(output)

	// Wrong password.
	if strings.Contains(lower, "wrong password") ||
		strings.Contains(lower, "data error in encrypted file") ||
		strings.Contains(lower, "can not open encrypted archive") {
		return FailWrongPassword
	}

	// Disk full.
	if strings.Contains(lower, "disk full") ||
		strings.Contains(lower, "no space left on device") {
		return FailDiskFull
	}

	// CRC / corrupt.
	if strings.Contains(lower, "error: crc failed") ||
		strings.Contains(lower, "data error") {
		// Exclude "data error in encrypted file" which is already caught above.
		if !strings.Contains(lower, "encrypted") {
			return FailCorrupt
		}
	}

	// Not a valid archive.
	if strings.Contains(lower, "cannot open the file as") ||
		strings.Contains(lower, "is not supported archive") {
		return FailNotArchive
	}

	return FailUnknown
}
