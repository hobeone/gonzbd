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

	res, err := unpack.UnRAR(t.Context(), slog.Default(), archive, outDir, "", unpack.Options{})
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
	_, _ = unpack.UnRAR(t.Context(), slog.Default(), archive, t.TempDir(), "", opts)

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

// TestUnRAR_PasswordFlag verifies UnRAR builds the correct password flag
// from its password parameter: "-p-" (suppress interactive prompt) when
// empty, "-p<redacted>" (the real "-p<password>" flag, redacted for
// display-safe logging by formatCmdLine) when non-empty. Mutation testing
// flagged this branch as uncovered after the password-parameter refactor --
// every other direct UnRAR() call in this package passes an empty password,
// so nothing previously exercised the non-empty case at this layer
// (GoUnRAR/UnRAR password tests only cover the rarengine path, not the
// unrar subprocess). The literal password is intentionally never present
// in the OnCommand cmdline -- see formatCmdLine -- so this test asserts
// against the redacted form rather than the raw "-psecret" flag.
func TestUnRAR_PasswordFlag(t *testing.T) {
	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.rar",
		Parts:    []string{"/tmp/does-not-exist.rar"},
	}

	t.Run("empty password suppresses prompt", func(t *testing.T) {
		var captured string
		opts := unpack.Options{
			UnrarCommand: "/nonexistent/binary",
			OnCommand:    func(cmdLine string) { captured = cmdLine },
		}
		_, _ = unpack.UnRAR(t.Context(), slog.Default(), archive, t.TempDir(), "", opts)

		if !strings.Contains(captured, "-p-") {
			t.Errorf("cmdline missing -p- for empty password: %q", captured)
		}
		if strings.Contains(captured, "-p<redacted>") {
			t.Errorf("cmdline should not contain a redacted password flag when no password is set: %q", captured)
		}
	})

	t.Run("non-empty password sets the redacted password flag", func(t *testing.T) {
		var captured string
		opts := unpack.Options{
			UnrarCommand: "/nonexistent/binary",
			OnCommand:    func(cmdLine string) { captured = cmdLine },
		}
		_, _ = unpack.UnRAR(t.Context(), slog.Default(), archive, t.TempDir(), "secret", opts)

		if !strings.Contains(captured, "-p<redacted>") {
			t.Errorf("cmdline missing redacted password flag: %q", captured)
		}
		if strings.Contains(captured, "-psecret") {
			t.Errorf("cmdline must never contain the literal password: %q", captured)
		}
		if strings.Contains(captured, "-p-") {
			t.Errorf("cmdline should not contain -p- when a password is set: %q", captured)
		}
	})
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
	_, _ = unpack.UnRAR(t.Context(), slog.Default(), archive, t.TempDir(), "", opts)

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
