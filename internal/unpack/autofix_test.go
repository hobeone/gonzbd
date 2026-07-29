package unpack_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/hobeone/gonzbd/internal/testutil"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// writeScript creates an executable script file at path, creating parent
// directories as needed.
//
// The previous comment here claimed an "atomic temp-file + rename
// pattern to avoid ETXTBSY under race"; the code did no rename, and a
// rename would not have helped -- it preserves the inode, which is what
// execve checks for write descriptors. testutil.WriteExecutable closes
// the real window; see there and issue #239.
func writeScript(t *testing.T, path string, content []byte) {
	t.Helper()
	testutil.WriteExecutable(t, path, string(content))
}

// TestUnRAR_CannotCreateAutoFix verifies (N4) that when unrar output
// contains "Cannot create" AND "Attempting to correct" AND files were
// extracted, the error is downgraded and res.Err is cleared.
func TestUnRAR_CannotCreateAutoFix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()

	dir := t.TempDir()
	outDir := t.TempDir()

	// Create a mock unrar script that:
	// 1. Prints "Cannot create" and "Attempting to correct the filename"
	// 2. Creates an output file (simulating successful extraction)
	// 3. Exits with code 1 (non-zero)
	scriptPath := filepath.Join(dir, "mock-unrar")
	script := `#!/bin/sh
# Parse the output dir from args — it's the last argument (with trailing /).
# Use POSIX eval form because dash doesn't support bash's ${@: -1}.
eval "OUTDIR=\${$#}"
echo "Extracting from archive.rar"
echo "Cannot create file.txt"
echo "Attempting to correct the filename"
echo "Creating file_corrected.txt OK"
# Create a file to simulate extraction
touch "${OUTDIR}file_corrected.txt"
exit 1
`
	writeScript(t, scriptPath, []byte(script))

	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "archive",
		MainFile: filepath.Join(dir, "archive.rar"),
		Parts:    []string{filepath.Join(dir, "archive.rar")},
	}
	// Create a dummy archive file.
	os.WriteFile(archive.MainFile, []byte("rar"), 0o644)

	res, err := unpack.UnRAR(t.Context(), slog.Default(), archive, outDir, "", unpack.Options{
		UnrarCommand: scriptPath,
	})

	// N4: Error should be cleared because extraction succeeded with corrections.
	if err != nil {
		t.Errorf("expected nil error after auto-fix, got: %v", err)
	}
	if res.Err != nil {
		t.Errorf("expected nil res.Err after auto-fix, got: %v", res.Err)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected ExitCode=0 after auto-fix, got: %d", res.ExitCode)
	}
	// Verify the extracted file exists.
	if _, err := os.Stat(filepath.Join(outDir, "file_corrected.txt")); err != nil {
		t.Errorf("extracted file should exist: %v", err)
	}
}

// TestUnRAR_CannotCreateNoAutoFix verifies that "Cannot create"
// WITHOUT "Attempting to correct" keeps the error.
func TestUnRAR_CannotCreateNoAutoFix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()

	dir := t.TempDir()
	outDir := t.TempDir()

	scriptPath := filepath.Join(dir, "mock-unrar")
	script := `#!/bin/sh
echo "Cannot create file.txt"
echo "Permission denied"
exit 1
`
	writeScript(t, scriptPath, []byte(script))

	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "archive",
		MainFile: filepath.Join(dir, "archive.rar"),
		Parts:    []string{filepath.Join(dir, "archive.rar")},
	}
	os.WriteFile(archive.MainFile, []byte("rar"), 0o644)

	_, err := unpack.UnRAR(t.Context(), slog.Default(), archive, outDir, "", unpack.Options{
		UnrarCommand: scriptPath,
	})

	// Without "Attempting to correct", the error should NOT be auto-fixed.
	if err == nil {
		t.Error("expected error to persist when no auto-correction happened")
	}
}

func TestUnRAR_CannotCreateNoAutoFix_ZeroFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test not portable to Windows")
	}
	t.Parallel()

	dir := t.TempDir()
	outDir := t.TempDir()

	scriptPath := filepath.Join(dir, "mock-unrar")
	script := `#!/bin/sh
echo "Cannot create file.txt"
echo "Attempting to correct the filename"
exit 1
`
	writeScript(t, scriptPath, []byte(script))

	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "archive",
		MainFile: filepath.Join(dir, "archive.rar"),
		Parts:    []string{filepath.Join(dir, "archive.rar")},
	}
	os.WriteFile(archive.MainFile, []byte("rar"), 0o644)

	_, err := unpack.UnRAR(t.Context(), slog.Default(), archive, outDir, "", unpack.Options{
		UnrarCommand: scriptPath,
	})

	// Since 0 files were extracted, it should NOT auto-correct and the error must persist.
	if err == nil {
		t.Error("expected error to persist when zero files were extracted")
	}
}
