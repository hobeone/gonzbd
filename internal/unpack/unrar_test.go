package unpack_test

import (
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

func TestUnRAR_Integration(t *testing.T) {
	if _, err := exec.LookPath("unrar"); err != nil {
		t.Skip("unrar binary not found in PATH; skipping integration test")
	}

	// We ship a small pre-generated test.rar in testdata/.
	// If the file doesn't exist, skip rather than fail.
	rarPath := "testdata/test.rar"
	if _, err := os.Stat(rarPath); err != nil {
		t.Skipf("testdata/test.rar not present (%v); skipping unrar integration test", err)
	}

	outDir := t.TempDir()
	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "test",
		MainFile: rarPath,
		Parts:    []string{rarPath},
	}

	res, err := unpack.UnRAR(t.Context(), slog.Default(), archive, outDir, unpack.Options{})
	if err != nil {
		t.Fatalf("UnRAR error: %v\nOutput:\n%s", err, res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("UnRAR exit code %d\nOutput:\n%s", res.ExitCode, res.Output)
	}
}

// TestUnRAR_OnCommandFiresBeforeExec verifies the OnCommand callback is
// invoked with the full argv before exec runs. Uses a non-existent binary
// so the test doesn't depend on unrar being installed; OnCommand fires
// before cmd.Run, so the test works whether or not unrar is on the host.
func TestUnRAR_OnCommandFiresBeforeExec(t *testing.T) {
	var captured string
	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.rar",
		Parts:    []string{"/tmp/does-not-exist.rar"},
	}
	opts := unpack.Options{
		UnrarCommand: "/nonexistent/binary",
		OnCommand: func(cmdLine string) {
			captured = cmdLine
		},
	}
	// Run is expected to fail; we don't check err.
	_, _ = unpack.UnRAR(t.Context(), slog.Default(), archive, t.TempDir(), opts)

	if captured == "" {
		t.Fatal("OnCommand was not called")
	}
	if !strings.Contains(captured, "/nonexistent/binary") {
		t.Errorf("cmdline missing binary: %q", captured)
	}
	if !strings.Contains(captured, archive.MainFile) {
		t.Errorf("cmdline missing archive path: %q", captured)
	}
	// Must contain the canonical unrar flags so we know it's the real argv,
	// not a stub formatted by the caller.
	for _, flag := range []string{"-y", "-idp", "-scf"} {
		if !strings.Contains(captured, flag) {
			t.Errorf("cmdline missing flag %q: %q", flag, captured)
		}
	}
}

// TestUnRAR_HasProblemDegradedMode verifies that when HasProblem is true
// (non-original or old unrar), the flags -scf, -or, -ai, -tsm- are NOT
// emitted. Matches SABnzbd's RAR_PROBLEM degraded mode.
func TestUnRAR_HasProblemDegradedMode(t *testing.T) {
	var captured string
	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.rar",
		Parts:    []string{"/tmp/does-not-exist.rar"},
	}
	opts := unpack.Options{
		UnrarCommand:     "/nonexistent/binary",
		HasProblem:       true,
		IgnoreUnrarDates: true, // should be suppressed in degraded mode
		OnCommand: func(cmdLine string) {
			captured = cmdLine
		},
	}
	_, _ = unpack.UnRAR(t.Context(), slog.Default(), archive, t.TempDir(), opts)

	if captured == "" {
		t.Fatal("OnCommand was not called")
	}

	// These flags should be present (always used):
	for _, flag := range []string{"-y", "-idp", "-o-"} {
		if !strings.Contains(captured, flag) {
			t.Errorf("degraded mode: cmdline missing required flag %q: %q", flag, captured)
		}
	}

	// These flags should be ABSENT in degraded mode:
	for _, flag := range []string{"-scf", "-ai", "-or", "-tsm-"} {
		if strings.Contains(captured, flag) {
			t.Errorf("degraded mode: cmdline should NOT contain flag %q but does: %q", flag, captured)
		}
	}
}
