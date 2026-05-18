package unpack

import (
	"context"
	"log/slog"
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
	if err != ErrWrongPassword {
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
	if err == ErrWrongPassword {
		t.Fatal("GoSevenZipWithPasswords: got ErrWrongPassword, expected context error")
	}
}
