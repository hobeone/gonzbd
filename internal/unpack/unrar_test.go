package unpack_test

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/cmdutil"
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

type logRecorder struct {
	attrs   []slog.Attr
	records *[]slog.Record
}

func newTestLogger() (*slog.Logger, *[]slog.Record) {
	records := &[]slog.Record{}
	h := &logRecorder{records: records}
	return slog.New(h), records
}

func (h *logRecorder) Enabled(ctx context.Context, l slog.Level) bool { return true }
func (h *logRecorder) Handle(ctx context.Context, r slog.Record) error {
	for _, a := range h.attrs {
		r.AddAttrs(a)
	}
	*h.records = append(*h.records, r)
	return nil
}
func (h *logRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &logRecorder{attrs: newAttrs, records: h.records}
}
func (h *logRecorder) WithGroup(name string) slog.Handler { return h }

func TestUnRAR_LoggedArgsRedacted(t *testing.T) {
	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.rar",
		Parts:    []string{"/tmp/does-not-exist.rar"},
	}

	t.Run("non-empty password is redacted in args log attribute", func(t *testing.T) {
		opts := unpack.Options{
			UnrarCommand: "/nonexistent/binary",
			ExtraArgs:    []string{"-v"},
		}
		logger, records := newTestLogger()
		_, _ = unpack.UnRAR(t.Context(), logger, archive, t.TempDir(), "supersecret", opts)

		var loggedArgs []string
		for _, r := range *records {
			if r.Message == "unrar: starting extraction" {
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

		var foundRedacted, foundExtra bool
		for _, arg := range loggedArgs {
			if arg == "-p<redacted>" {
				foundRedacted = true
			}
			if arg == "-v" {
				foundExtra = true
			}
		}
		if !foundRedacted {
			t.Errorf("logged args missing -p<redacted> flag: %v", loggedArgs)
		}
		if !foundExtra {
			t.Errorf("logged args missing extra arg -v: %v", loggedArgs)
		}
	})

	t.Run("empty password retains -p- without redaction in args log attribute", func(t *testing.T) {
		opts := unpack.Options{
			UnrarCommand: "/nonexistent/binary",
		}
		logger, records := newTestLogger()
		_, _ = unpack.UnRAR(t.Context(), logger, archive, t.TempDir(), "", opts)

		var loggedArgs []string
		for _, r := range *records {
			if r.Message == "unrar: starting extraction" {
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

		var foundPromptSuppress bool
		for _, arg := range loggedArgs {
			if arg == "-p-" {
				foundPromptSuppress = true
			}
			if arg == "-p<redacted>" {
				t.Errorf("logged args should not contain -p<redacted> when password is empty: %v", loggedArgs)
			}
		}
		if !foundPromptSuppress {
			t.Errorf("logged args missing -p- flag: %v", loggedArgs)
		}
	})
}

func TestUnRAR_SandboxOptions(t *testing.T) {
	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.rar",
		Parts:    []string{"/tmp/does-not-exist.rar"},
	}
	opts := unpack.Options{
		UseGoRAR: false,
		Sandbox: cmdutil.SandboxConfig{
			Enabled:   true,
			Strict:    true,
			TargetDir: "/tmp",
		},
	}
	res, err := unpack.UnRAR(t.Context(), slog.Default(), archive, "/tmp", "", opts)
	if err == nil {
		t.Fatalf("expected error when strict sandbox is enabled on non-existent or unsupported binary, got nil (res=%+v)", res)
	}
}

func TestUnrarBin(t *testing.T) {
	t.Run("explicit command override", func(t *testing.T) {
		got, err := unpack.UnrarBin(unpack.Options{UnrarCommand: "/custom/unrar"})
		if err != nil || got != "/custom/unrar" {
			t.Errorf("UnrarBin() = (%q, %v), want (\"/custom/unrar\", nil)", got, err)
		}
	})
	t.Run("env var override", func(t *testing.T) {
		t.Setenv("GONZBD_UNRAR_BIN", "/env/unrar")
		got, err := unpack.UnrarBin(unpack.Options{})
		if err != nil || got != "/env/unrar" {
			t.Errorf("UnrarBin() = (%q, %v), want (\"/env/unrar\", nil)", got, err)
		}
	})
	t.Run("not found in path", func(t *testing.T) {
		t.Setenv("PATH", "")
		_, err := unpack.UnrarBin(unpack.Options{})
		if err == nil {
			t.Error("expected error when unrar not in PATH and no override set")
		}
	})
}
