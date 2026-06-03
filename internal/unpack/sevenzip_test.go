package unpack_test

import (
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

func TestSevenZip_Integration(t *testing.T) {
	var bin string
	var err error
	for _, name := range unpack.SevenZipBinaries {
		if bin, err = exec.LookPath(name); err == nil {
			break
		}
	}
	if err != nil || bin == "" {
		t.Skip("7zz/7z binary not found in PATH; skipping integration test")
	}

	// We ship a small pre-generated test.7z in testdata/.
	szPath := "testdata/test.7z"
	if _, err := os.Stat(szPath); err != nil {
		t.Skipf("testdata/test.7z not present (%v); skipping 7zip integration test", err)
	}

	outDir := t.TempDir()
	archive := unpack.Archive{
		Type:     unpack.SevenZipArchive,
		Name:     "test",
		MainFile: szPath,
		Parts:    []string{szPath},
	}

	res, err := unpack.SevenZip(t.Context(), slog.Default(), archive, outDir, unpack.Options{})
	if err != nil {
		t.Fatalf("SevenZip error: %v\nOutput:\n%s", err, res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("SevenZip exit code %d\nOutput:\n%s", res.ExitCode, res.Output)
	}
}

// TestSevenZip_OnCommandFiresBeforeExec mirrors the unrar test for 7z.
func TestSevenZip_OnCommandFiresBeforeExec(t *testing.T) {
	var captured string
	archive := unpack.Archive{
		Type:     unpack.SevenZipArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.7z",
		Parts:    []string{"/tmp/does-not-exist.7z"},
	}
	opts := unpack.Options{
		SevenZipCommand: "/nonexistent/binary",
		OnCommand: func(cmdLine string) {
			captured = cmdLine
		},
	}
	_, _ = unpack.SevenZip(t.Context(), slog.Default(), archive, t.TempDir(), opts)

	if captured == "" {
		t.Fatal("OnCommand was not called")
	}
	if !strings.Contains(captured, archive.MainFile) {
		t.Errorf("cmdline missing archive path: %q", captured)
	}
}
