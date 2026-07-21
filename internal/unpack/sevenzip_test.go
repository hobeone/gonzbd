package unpack_test

import (
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/cmdutil"
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
		if os.Getenv("CI") != "" {
			t.Fatalf("7zz/7z binary not found in PATH in CI environment; required for integration test")
		}
		t.Skip("7zz/7z binary not found in PATH; skipping integration test")
	}

	// Use our committed valid 7z fixture in testdata/.
	// Fail loudly if missing since it is a committed repository fixture.
	szPath := "testdata/sevenzip/copy.7z"
	if _, err := os.Stat(szPath); err != nil {
		t.Fatalf("testdata/sevenzip/copy.7z not present (%v); fixture required", err)
	}

	outDir := t.TempDir()
	archive := unpack.Archive{
		Type:     unpack.SevenZipArchive,
		Name:     "test",
		MainFile: szPath,
		Parts:    []string{szPath},
	}

	res, err := unpack.SevenZip(t.Context(), slog.Default(), archive, outDir, "", unpack.Options{})
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
	_, _ = unpack.SevenZip(t.Context(), slog.Default(), archive, t.TempDir(), "", opts)

	if captured == "" {
		t.Fatal("OnCommand was not called")
	}
	if !strings.Contains(captured, archive.MainFile) {
		t.Errorf("cmdline missing archive path: %q", captured)
	}
	if !strings.Contains(captured, opts.SevenZipCommand) {
		t.Errorf("cmdline missing custom binary path: %q", captured)
	}
}

func TestSevenZip_LoggedArgsRedacted(t *testing.T) {
	archive := unpack.Archive{
		Type:     unpack.SevenZipArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.7z",
		Parts:    []string{"/tmp/does-not-exist.7z"},
	}

	t.Run("non-empty password is redacted in args log attribute", func(t *testing.T) {
		opts := unpack.Options{
			SevenZipCommand: "/nonexistent/binary",
		}
		logger, records := newTestLogger()
		_, _ = unpack.SevenZip(t.Context(), logger, archive, t.TempDir(), "supersecret", opts)

		var loggedArgs []string
		for _, r := range *records {
			if r.Message == "7zip: starting extraction" {
				r.Attrs(func(a slog.Attr) bool {
					if a.Key == "args" {
						if slice, ok := a.Value.Any().([]string); ok {
							loggedArgs = slice
						}
					}
					return true
				})
			}
		}

		if loggedArgs == nil {
			t.Fatal("did not find args attribute of type []string in starting extraction log record")
		}

		for _, arg := range loggedArgs {
			if strings.Contains(arg, "supersecret") {
				t.Errorf("logged args leaked secret password: %q in %v", arg, loggedArgs)
			}
		}

		var foundRedacted bool
		for _, arg := range loggedArgs {
			if arg == "-p<redacted>" {
				foundRedacted = true
			}
		}
		if !foundRedacted {
			t.Errorf("logged args missing -p<redacted> flag: %v", loggedArgs)
		}
	})
}

func TestSevenZip_Coverage(t *testing.T) {
	archive := unpack.Archive{
		Type:     unpack.SevenZipArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.7z",
		Parts:    []string{"/tmp/does-not-exist.7z"},
	}

	var onlineLines []string
	opts := unpack.Options{
		SevenZipCommand: "/nonexistent/binary",
		OneFolder:       true,
		OverwriteFiles:  true,
		OnLine: func(l string) {
			onlineLines = append(onlineLines, l)
		},
	}
	_, _ = unpack.SevenZip(t.Context(), slog.Default(), archive, t.TempDir(), "", opts)
	if len(onlineLines) == 0 {
		t.Error("expected OnLine to be called")
	}

	if falseBin, err := exec.LookPath("false"); err == nil {
		res, err := unpack.SevenZip(t.Context(), slog.Default(), archive, t.TempDir(), "", unpack.Options{
			SevenZipCommand: falseBin,
		})
		if err == nil {
			t.Error("expected error from false command")
		}
		if res.ExitCode == 0 {
			t.Errorf("expected non-zero exit code, got %d", res.ExitCode)
		}
	}
}

func TestSevenZip_SandboxOptions(t *testing.T) {
	archive := unpack.Archive{
		Type:     unpack.SevenZipArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.7z",
		Parts:    []string{"/tmp/does-not-exist.7z"},
	}
	opts := unpack.Options{
		UseGo7z: false,
		Sandbox: cmdutil.SandboxConfig{
			Enabled:   true,
			Strict:    true,
			TargetDir: "/tmp",
		},
	}
	res, err := unpack.SevenZip(t.Context(), slog.Default(), archive, "/tmp", "", opts)
	if err == nil {
		t.Fatalf("expected error when strict sandbox is enabled on non-existent or unsupported binary, got nil (res=%+v)", res)
	}
}
