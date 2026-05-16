package unpack

import (
	"context"
	"errors"
	"log/slog"
	"strings"
)

// ErrWrongPassword is returned when all passwords in the list have been
// exhausted without successful extraction.
var ErrWrongPassword = errors.New("unpack: wrong password (all passwords exhausted)")

// extractFunc is the signature shared by UnRAR and SevenZip — the
// tool-specific extraction function called by withPasswords.
type extractFunc func(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error)

// wrongPasswordFunc inspects a tool's exit code and captured output
// to determine whether the failure was caused by a wrong password.
type wrongPasswordFunc func(exitCode int, output string) bool

// isUnrarWrongPassword returns true if the unrar output or exit code
// indicates a wrong password was used.
// unrar exit code 2 = "fatal error" which covers wrong passwords.
// The output message is more reliable than the exit code.
func isUnrarWrongPassword(exitCode int, output string) bool {
	// unrar prints various messages for wrong passwords:
	//   "Incorrect password for ..."
	//   "Checksum error in the encrypted file"
	//   "The specified password is incorrect."
	//   "Encrypted file: CRC failed in ..."
	lower := strings.ToLower(output)
	if strings.Contains(lower, "incorrect password") ||
		strings.Contains(lower, "the specified password is incorrect") ||
		strings.Contains(lower, "checksum error in the encrypted file") ||
		strings.Contains(lower, "encrypted file: crc failed") {
		return true
	}
	// Exit code 2 is a generic "fatal error" that often means wrong password
	// when combined with encrypted archives, but we rely on output parsing
	// above for precision.
	return false
}

// is7zWrongPassword returns true if the 7z output indicates a wrong
// password was used.
func is7zWrongPassword(exitCode int, output string) bool {
	// 7z prints:
	//   "Data Error in encrypted file. Wrong password?"
	//   "Can not open encrypted archive. Wrong password?"
	//   "Wrong password?"
	lower := strings.ToLower(output)
	return strings.Contains(lower, "wrong password") ||
		strings.Contains(lower, "data error in encrypted file")
}

// allPasswords returns the effective password list from Options.
// Priority: Passwords list first, then single Password field (for
// backward compatibility). Duplicates are removed.
func allPasswords(opts Options) []string {
	seen := make(map[string]bool)
	var result []string
	for _, p := range opts.Passwords {
		if p != "" && !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	if opts.Password != "" && !seen[opts.Password] {
		result = append(result, opts.Password)
	}
	return result
}

// withPasswords is the generic password-iteration loop used by both
// UnRARWithPasswords and SevenZipWithPasswords. It tries each password
// from allPasswords(opts) in order, calling extract for each attempt.
// isWrongPW classifies whether a failure was due to a wrong password
// (retry) vs. something else (abort).
func withPasswords(
	ctx context.Context,
	log *slog.Logger,
	archive Archive,
	outDir string,
	opts Options,
	extract extractFunc,
	isWrongPW wrongPasswordFunc,
	toolName string,
) (Result, error) {
	passwords := allPasswords(opts)
	if len(passwords) == 0 {
		// No password list — single attempt (no-password).
		return extract(ctx, log, archive, outDir, opts)
	}

	var lastRes Result
	for i, pw := range passwords {
		attempt := opts
		attempt.Password = pw
		res, err := extract(ctx, log, archive, outDir, attempt)

		// System-level error (binary not found, context cancelled).
		if err != nil && res.ExitCode == 0 {
			return res, err
		}

		// Success.
		if err == nil {
			if i > 0 {
				log.Info(toolName+": password found", "attempt", i+1, "archive", archive.MainFile)
			}
			return res, nil
		}

		// Check if wrong password — try next.
		// Check res.Reason first (for GoUnRAR which classifies errors directly),
		// then fall back to isWrongPW callback (for subprocess output parsing).
		if res.Reason == FailWrongPassword || isWrongPW(res.ExitCode, res.Output) {
			log.Info(toolName+": wrong password, trying next",
				"attempt", i+1, "total", len(passwords), "archive", archive.MainFile)
			lastRes = res
			continue
		}

		// Other extraction error (corrupt archive, disk full, etc.).
		return res, err
	}

	// All passwords exhausted.
	lastRes.Err = ErrWrongPassword
	return lastRes, ErrWrongPassword
}

// UnRARWithPasswords tries extracting with each password in opts.Passwords
// (and opts.Password) until one succeeds or all are exhausted. If the
// archive is not password-protected, the first attempt with no password
// succeeds immediately.
//
// When no passwords are configured, this delegates directly to UnRAR.
func UnRARWithPasswords(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
	return withPasswords(ctx, log, archive, outDir, opts, UnRAR, isUnrarWrongPassword, "unrar")
}

// GoUnRARWithPasswords tries extracting with each password in opts.Passwords
// (and opts.Password) using the pure-Go rardecode extractor until one
// succeeds or all are exhausted.
//
// When no passwords are configured, this delegates directly to GoUnRAR.
func GoUnRARWithPasswords(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
	// GoUnRAR has no subprocess output, so isWrongPW always returns false.
	// Wrong-password detection works via res.Reason == FailWrongPassword
	// in the withPasswords loop.
	never := func(int, string) bool { return false }
	return withPasswords(ctx, log, archive, outDir, opts, GoUnRAR, never, "go_unrar")
}

// SevenZipWithPasswords tries extracting with each password in opts.Passwords
// (and opts.Password) until one succeeds or all are exhausted.
//
// When no passwords are configured, this delegates directly to SevenZip.
func SevenZipWithPasswords(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
	return withPasswords(ctx, log, archive, outDir, opts, SevenZip, is7zWrongPassword, "7zip")
}

// GoSevenZipWithPasswords tries extracting with each password using the
// pure-Go sevenzip extractor until one succeeds or all are exhausted.
//
// When no passwords are configured, this delegates directly to GoSevenZip.
func GoSevenZipWithPasswords(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
	// GoSevenZip has no subprocess output, so isWrongPW always returns false.
	// Wrong-password detection works via res.Reason == FailWrongPassword
	// in the withPasswords loop.
	never := func(int, string) bool { return false }
	return withPasswords(ctx, log, archive, outDir, opts, GoSevenZip, never, "go_7z")
}
