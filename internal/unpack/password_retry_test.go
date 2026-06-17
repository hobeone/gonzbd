package unpack

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestGoSevenZipWithPasswords_RetriesOnWrongPassword verifies that
// password iteration works correctly for the Go-native 7z extractor.
// The correct password is placed last in the list — all earlier wrong
// passwords must be tried and discarded before the correct one succeeds.
//
// This is a regression test for C3: the ExitCode==0 guard in
// withPasswords caused every Go extractor error to early-return,
// bypassing the wrong-password retry logic entirely.
func TestGoSevenZipWithPasswords_RetriesOnWrongPassword(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "aes7z.7z"),
	}

	opts := Options{
		Passwords: []string{"wrong1", "wrong2", "password"},
	}

	res, err := GoSevenZipWithPasswords(context.Background(), slog.Default(), archive, outDir, opts)
	if err != nil {
		t.Fatalf("GoSevenZipWithPasswords: expected success with correct password last, got error: %v (reason: %v)", err, res.Reason)
	}
	if len(res.ExtractedFiles) == 0 {
		t.Fatal("GoSevenZipWithPasswords: no files extracted")
	}
	t.Logf("Extracted %d files after password retry", len(res.ExtractedFiles))
}

// TestGoSevenZipWithPasswords_AllWrongExhausted verifies that when all
// passwords are wrong, the error is ErrWrongPassword (not a system error).
func TestGoSevenZipWithPasswords_AllWrongExhausted(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "aes7z.7z"),
	}

	opts := Options{
		Passwords: []string{"wrong1", "wrong2", "wrong3"},
	}

	_, err := GoSevenZipWithPasswords(context.Background(), slog.Default(), archive, outDir, opts)
	if err == nil {
		t.Fatal("GoSevenZipWithPasswords: expected error when all passwords wrong")
	}
	if !errors.Is(err, ErrWrongPassword) {
		t.Errorf("GoSevenZipWithPasswords: expected ErrWrongPassword, got: %v", err)
	}
}

// TestGoSevenZipWithPasswords_SystemErrorStillEarlyReturns verifies that
// genuine system errors (context cancelled) still cause an immediate
// return without trying further passwords.
func TestGoSevenZipWithPasswords_SystemErrorStillEarlyReturns(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "aes7z.7z"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	opts := Options{
		Passwords: []string{"wrong1", "password"},
	}

	_, err := GoSevenZipWithPasswords(ctx, slog.Default(), archive, outDir, opts)
	if err == nil {
		t.Fatal("GoSevenZipWithPasswords: expected error for cancelled context")
	}
	// Should be context error, NOT ErrWrongPassword.
	if errors.Is(err, ErrWrongPassword) {
		t.Fatal("GoSevenZipWithPasswords: got ErrWrongPassword, expected context error")
	}
}

// TestGoSevenZipWithPasswords_CleanupPartialFiles verifies that partial
// files from failed password attempts are removed before retrying with
// the next password (S2 fix). Without cleanup, OverwriteFiles=false
// would skip corrupt partials on the correct password attempt.
func TestGoSevenZipWithPasswords_CleanupPartialFiles(t *testing.T) {
	td := sevenZipTestdata(t)
	outDir := t.TempDir()
	archive := Archive{
		Type:     SevenZipArchive,
		MainFile: filepath.Join(td, "aes7z.7z"),
	}

	// Use OverwriteFiles=false (the default) which exposes the bug:
	// without cleanup, partial files from wrong passwords survive and
	// get skipped by the correct password attempt.
	opts := Options{
		Passwords:      []string{"wrong1", "password"},
		OverwriteFiles: false,
	}

	res, err := GoSevenZipWithPasswords(context.Background(), slog.Default(), archive, outDir, opts)
	if err != nil {
		t.Fatalf("expected success, got error: %v (reason: %v)", err, res.Reason)
	}

	// Verify all extracted files have non-zero size (i.e. they contain
	// real data from the correct password, not garbage from the wrong one).
	for _, rel := range res.ExtractedFiles {
		full := filepath.Join(outDir, rel)
		info, err := os.Stat(full)
		if err != nil {
			t.Errorf("extracted file %s not found: %v", rel, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("extracted file %s is empty (possible corrupt partial from wrong password)", rel)
		}
	}
}
