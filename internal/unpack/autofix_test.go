package unpack_test

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
// extracted, the error is downgraded and nil is returned.
func TestUnRAR_CannotCreateAutoFix(t *testing.T) {
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
	if res.ExitCode != 0 {
		t.Errorf("expected ExitCode=0 after auto-fix, got: %d", res.ExitCode)
	}
	// Verify the extracted file exists.
	if _, err := os.Stat(filepath.Join(outDir, "file_corrected.txt")); err != nil {
		t.Errorf("extracted file should exist: %v", err)
	}
}

// TestUnRAR_CannotCreateAutoFixFlatRenamesTakenName verifies that the
// auto-fix path publishes a flat extraction as the success path does: a name
// outDir already holds is published under a "_N" name, not skipped (#897).
func TestUnRAR_CannotCreateAutoFixFlatRenamesTakenName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "file_corrected.txt"), []byte("pre"), 0o600); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(dir, "mock-unrar")
	script := `#!/bin/sh
eval "OUTDIR=\${$#}"
echo "Cannot create file.txt"
echo "Attempting to correct the filename"
printf member > "${OUTDIR}file_corrected.txt"
exit 1
`
	writeScript(t, scriptPath, []byte(script))
	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "archive",
		MainFile: filepath.Join(dir, "archive.rar"),
		Parts:    []string{filepath.Join(dir, "archive.rar")},
	}

	res, err := unpack.UnRAR(t.Context(), slog.Default(), archive, outDir, "", unpack.Options{
		UnrarCommand: scriptPath,
		OneFolder:    true,
	})
	if err != nil {
		t.Fatalf("UnRAR: %v", err)
	}
	if len(res.ExtractedFiles) != 1 || res.ExtractedFiles[0] != "file_corrected_1.txt" {
		t.Errorf("ExtractedFiles = %q, want [file_corrected_1.txt]", res.ExtractedFiles)
	}
	for name, want := range map[string]string{"file_corrected.txt": "pre", "file_corrected_1.txt": "member"} {
		if got, err := os.ReadFile(filepath.Join(outDir, name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
}

// TestUnRAR_CannotCreateNoAutoFix verifies that "Cannot create"
// WITHOUT "Attempting to correct" keeps the error.
func TestUnRAR_CannotCreateNoAutoFix(t *testing.T) {
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

// TestUnRAR_UnreadableStageIsReportedAndLogged pins two failures around the
// auto-correction check that used to vanish: a staging directory that cannot
// be read is part of the returned error, and one that cannot be removed
// afterwards is logged.
func TestUnRAR_UnreadableStageIsReportedAndLogged(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permission this test relies on")
	}

	dir := t.TempDir()
	outDir := t.TempDir()
	// Registered after TempDir, so it runs first and lets TempDir remove
	// the locked directory the script leaves behind. WalkDir visits a
	// directory before reading it, so each is unlocked in time.
	t.Cleanup(func() {
		_ = filepath.WalkDir(outDir, func(path string, d fs.DirEntry, _ error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})

	// The locked directory makes the stage unreadable. It no longer keeps
	// the stage from being removed (#862), so the script also takes write
	// permission away from outDir, the stage's parent.
	scriptPath := filepath.Join(dir, "mock-unrar")
	script := `#!/bin/sh
eval "OUTDIR=\${$#}"
echo "Cannot create file.txt"
echo "Attempting to correct the filename"
touch "${OUTDIR}file_corrected.txt"
mkdir "${OUTDIR}locked"
touch "${OUTDIR}locked/inner"
chmod 000 "${OUTDIR}locked"
chmod 555 "${OUTDIR}.."
exit 1
`
	writeScript(t, scriptPath, []byte(script))

	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "archive",
		MainFile: filepath.Join(dir, "archive.rar"),
		Parts:    []string{filepath.Join(dir, "archive.rar")},
	}
	if err := os.WriteFile(archive.MainFile, []byte("rar"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	_, err := unpack.UnRAR(t.Context(), slog.New(slog.NewTextHandler(&logs, nil)), archive, outDir, "", unpack.Options{
		UnrarCommand: scriptPath,
	})
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want it to carry the unreadable staging directory", err)
	}
	if !strings.Contains(logs.String(), `msg="unpack: failed to remove the staging directory"`) {
		t.Errorf("a staging directory left behind was not logged; log:\n%s", logs.String())
	}
}
