package unpack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// preVerifyFunc cheaply checks a password candidate against the archive's
// own embedded check value, without spawning any extraction subprocess.
// It returns skip=true only when it can positively prove the candidate is
// wrong; any uncertainty (parse error, no check value present, not a
// supported format) must return false so the real extraction attempt
// still runs.
type preVerifyFunc func(archive Archive, password string) (skip bool)

// rarPreVerify implements preVerifyFunc for RAR5 archives using
// rarheader.VerifyPassword's cheap embedded-check-value comparison. It is
// wired into UnRARWithPasswords only (the external-unrar-subprocess path) —
// GoUnRARWithPasswords doesn't need it: rarengine already verifies the
// password against the same check value lazily, inline, during
// decompression setup, failing fast on the very first Read() without
// spawning any subprocess.
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
	// 7z prints:
	//   "Data Error in encrypted file. Wrong password?"
	//   "Can not open encrypted archive. Wrong password?"
	//   "Wrong password?"
	lower := strings.ToLower(output)
	return strings.Contains(lower, "wrong password") ||
		strings.Contains(lower, "data error in encrypted file")
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
	preVerify preVerifyFunc,
) (Result, error) {
	passwords := allPasswords(opts)
	if len(passwords) == 0 {
		// No password list — single attempt (no-password).
		res, err := extract(ctx, log, archive, outDir, "", opts)
		res.Engine = toolName
		return res, err
	}

	var lastRes Result
	for i, pw := range passwords {
		if preVerify != nil && preVerify(archive, pw) {
			// Provably wrong against the archive's own embedded check
			// value — skip the real extraction attempt (subprocess spawn,
			// for external tools) entirely.
			log.Info(toolName+": skipping known-wrong password (pre-verified), trying next",
				"attempt", i+1, "total", len(passwords), "archive", archive.MainFile)
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("Skipping known-wrong password (attempt %d/%d), trying next…", i+1, len(passwords)))
			}
			lastRes = Result{Engine: toolName, Reason: FailWrongPassword}
			continue
		}

		// Snapshot outDir before attempt so we can clean up partial
		// files if this password turns out to be wrong (S2 fix).
		beforeSnap, _ := snapshotDir(outDir)

		res, err := extract(ctx, log, archive, outDir, pw, opts)
		res.Engine = toolName

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
		if res.Reason == FailWrongPassword || isWrongPW(res.ExitCode, res.Output) {
			// Clean up any partial files from this failed attempt.
			// Without this, OverwriteFiles=false would skip corrupt
			// partials on the next attempt with the correct password.
			cleanupPartialFiles(outDir, beforeSnap, log, toolName, i+1)

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
	lastRes.Err = ErrWrongPassword
	return lastRes, ErrWrongPassword
}

// cleanupPartialFiles removes files that were created during a failed
// password attempt. It diffs the current directory state against the
// pre-attempt snapshot and removes any new files.
func cleanupPartialFiles(outDir string, beforeSnap map[string]struct{}, log *slog.Logger, toolName string, attempt int) {
	afterSnap, err := snapshotDir(outDir)
	if err != nil {
		return
	}
	newFiles := diffSnapshot(beforeSnap, afterSnap)
	if len(newFiles) == 0 {
		return
	}
	for _, rel := range newFiles {
		full := filepath.Join(outDir, rel)
		if rmErr := os.Remove(full); rmErr == nil {
			log.Debug(toolName+": removed partial file from wrong password attempt",
				"file", rel, "attempt", attempt)
		}
	}
}

// UnRARWithPasswords tries extracting with each password in opts.Passwords
// until one succeeds or all are exhausted. If the archive is not
// password-protected, the first attempt with no password succeeds
// immediately.
//
// When no passwords are configured, this delegates directly to UnRAR.
func UnRARWithPasswords(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
	return withPasswords(ctx, log, archive, outDir, opts, UnRAR, isUnrarWrongPassword, "unrar", rarPreVerify)
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
	return withPasswords(ctx, log, archive, outDir, opts, GoUnRAR, never, "go_unrar", nil)
}

// SevenZipWithPasswords tries extracting with each password in opts.Passwords
// until one succeeds or all are exhausted.
//
// When no passwords are configured, this delegates directly to SevenZip.
func SevenZipWithPasswords(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
	return withPasswords(ctx, log, archive, outDir, opts, SevenZip, is7zWrongPassword, "7zip", nil)
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
	return withPasswords(ctx, log, archive, outDir, opts, GoSevenZip, never, "go_7z", nil)
}
