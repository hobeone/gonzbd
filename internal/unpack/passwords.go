package unpack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hobeone/gonzbd/internal/rarheader"
)

// ErrWrongPassword is returned when all passwords in the list have been
// exhausted without successful extraction.
var ErrWrongPassword = errors.New("unpack: wrong password (all passwords exhausted)")

// extractFunc is the signature shared by UnRAR, SevenZip, GoUnRAR, and
// GoSevenZip — the tool-specific extraction function called by
// withPasswords, once per password candidate.
type extractFunc func(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, opts Options) (Result, error)

// wrongPasswordFunc inspects a tool's exit code and captured output
// to determine whether the failure was caused by a wrong password.
type wrongPasswordFunc func(exitCode int, output string) bool

// rarPreVerify cheaply checks a password candidate against a RAR5 archive's
// own embedded check value (rarheader.VerifyPassword), without spawning any
// extraction subprocess. It returns skip=true only when it can positively
// prove the candidate wrong; any uncertainty (parse error, no check value
// present, not RAR5) returns false so the real extraction attempt still
// runs. UnRAR consults it before running unrar. GoUnRAR doesn't need it:
// rarengine verifies the password against the same check value during
// decompression setup, failing on the very first Read().
func rarPreVerify(archive Archive, password string) bool {
	verified, hasCheckValue, err := rarheader.VerifyPassword(archive.MainFile, password)
	if err != nil || !hasCheckValue {
		// Can't tell (parse error, no check value, or not RAR5) -- don't
		// skip; let the real subprocess attempt decide.
		return false
	}
	return !verified
}

// isUnrarWrongPassword returns true if the unrar output or exit code
// indicates a wrong password was used.
// unrar exit code 2 = "fatal error" which covers wrong passwords.
// The output message is more reliable than the exit code.
func isUnrarWrongPassword(exitCode int, output string) bool {
	return isUnrarPasswordError(strings.ToLower(output))
}

// is7zWrongPassword returns true if the 7z output indicates a wrong
// password was used.
func is7zWrongPassword(exitCode int, output string) bool {
	return Classify7zOutput(output) == FailWrongPassword
}

// allPasswords returns the effective password list from Options, with
// duplicates and empty entries removed.
func allPasswords(opts Options) []string {
	seen := make(map[string]bool)
	var result []string
	for _, p := range opts.Passwords {
		if p != "" && !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	return result
}

// withPasswords is the generic password-iteration loop used by
// UnRARWithPasswords, GoUnRARWithPasswords, SevenZipWithPasswords and
// GoSevenZipWithPasswords. It tries each password
// from allPasswords(opts) in order, calling extract for each attempt.
// isWrongPW classifies whether a failure was due to a wrong password
// (retry) vs. something else (abort). It returns the last attempt's Result
// as extract returned it; toolName only labels its log lines.
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
		return extract(ctx, log, archive, outDir, "", opts)
	}

	var lastRes Result
	for i, pw := range passwords {
		res, err := extract(ctx, log, archive, outDir, pw, opts)

		// System-level error: binary not found, context cancelled, etc.
		// For subprocess extractors, ExitCode==0 with an error means the
		// process failed to start (not an extraction failure). For Go-native
		// extractors, ExitCode is always 0, so we must also check res.Reason:
		// if the extractor already classified the error (e.g. FailWrongPassword,
		// FailCorrupt), it's an extraction failure, not a system error.
		if err != nil && res.ExitCode == 0 && res.Reason == FailUnknown {
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
		// A failed attempt left nothing in outDir (extractInStage), so the
		// next one starts from what was there before.
		if res.Reason == FailWrongPassword || isWrongPW(res.ExitCode, res.Output) {
			log.Info(toolName+": wrong password, trying next",
				"attempt", i+1, "total", len(passwords), "archive", archive.MainFile)
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("Wrong password (attempt %d/%d), trying next…", i+1, len(passwords)))
			}
			lastRes = res
			continue
		}

		// Other extraction error (corrupt archive, disk full, etc.).
		return res, err
	}

	// All passwords exhausted.
	return lastRes, ErrWrongPassword
}

// UnRARWithPasswords tries extracting with each password in opts.Passwords
// until one succeeds or all are exhausted. If the archive is not
// password-protected, the first attempt with no password succeeds
// immediately.
//
// When no passwords are configured, this delegates directly to UnRAR.
func UnRARWithPasswords(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
	return withPasswords(ctx, log, archive, outDir, opts, UnRAR, isUnrarWrongPassword, "unrar")
}

// GoUnRARWithPasswords tries extracting with each password in opts.Passwords
// using the pure-Go rarengine extractor until one succeeds or all are
// exhausted.
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
// until one succeeds or all are exhausted.
//
// When no passwords are configured, this delegates directly to SevenZip.
func SevenZipWithPasswords(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
	return withPasswords(ctx, log, archive, outDir, opts, SevenZip, is7zWrongPassword, "7z")
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
