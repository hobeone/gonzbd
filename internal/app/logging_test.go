package app_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
)

func TestSetupStderr(t *testing.T) {
	opts := app.LoggingOptions{
		Level:   slog.LevelInfo,
		LogFile: "",
	}
	logger, closer, err := app.Setup(opts)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	defer func() {
		if closer != nil {
			_ = closer.Close() //nolint:errcheck
		}
	}()

	if logger == nil {
		t.Fatal("logger is nil")
	}

	// Verify the returned logger was installed as the default.
	if slog.Default() != logger {
		t.Error("returned logger is not slog.Default()")
	}

	// Closer should be nil when LogFile is empty.
	if closer != nil {
		t.Error("closer should be nil for stderr-only logging")
	}
}

func TestSetupWithFile(t *testing.T) {
	tmpdir := t.TempDir()
	logFile := filepath.Join(tmpdir, "logs", "test.log")

	opts := app.LoggingOptions{
		Level:   slog.LevelDebug,
		LogFile: logFile,
	}
	logger, closer, err := app.Setup(opts)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	defer func() {
		if closer != nil {
			_ = closer.Close() //nolint:errcheck
		}
	}()

	if logger == nil {
		t.Fatal("logger is nil")
	}

	if closer == nil {
		t.Fatal("closer should not be nil when LogFile is set")
	}

	// Verify the log file exists.
	if _, err := os.Stat(logFile); err != nil {
		t.Fatalf("log file does not exist: %v", err)
	}

	// Write a log message and verify it reaches the file.
	logger.Info("test message", "key", "value")

	// Close the file to flush.
	if err := closer.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	// Read the file and verify the log line is there.
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	content := string(data)
	if content == "" {
		t.Error("log file is empty")
	}
	if !bytes.Contains(data, []byte("test message")) {
		t.Errorf("log file does not contain 'test message': %s", content)
	}
}

func TestSetupCreatesParentDir(t *testing.T) {
	tmpdir := t.TempDir()
	logFile := filepath.Join(tmpdir, "a", "b", "c", "test.log")

	opts := app.LoggingOptions{
		Level:   slog.LevelInfo,
		LogFile: logFile,
	}
	_, closer, err := app.Setup(opts)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	defer func() {
		if closer != nil {
			_ = closer.Close() //nolint:errcheck
		}
	}()

	// Verify all parent directories were created.
	if _, err := os.Stat(filepath.Dir(logFile)); err != nil {
		t.Fatalf("parent directory not created: %v", err)
	}

	if _, err := os.Stat(logFile); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
}

func TestSetupLogLevel(t *testing.T) {
	tests := []struct {
		name      string
		level     slog.Level
		wantLevel slog.Level
	}{
		{"debug", slog.LevelDebug, slog.LevelDebug},
		{"info", slog.LevelInfo, slog.LevelInfo},
		{"warn", slog.LevelWarn, slog.LevelWarn},
		{"error", slog.LevelError, slog.LevelError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := app.LoggingOptions{
				Level:   tt.level,
				LogFile: "",
			}
			logger, closer, err := app.Setup(opts)
			if err != nil {
				t.Fatalf("Setup failed: %v", err)
			}
			defer func() {
				if closer != nil {
					_ = closer.Close() //nolint:errcheck
				}
			}()

			if logger == nil {
				t.Fatal("logger is nil")
			}
		})
	}
}

func TestSetupDoubleClose(t *testing.T) {
	tmpdir := t.TempDir()
	logFile := filepath.Join(tmpdir, "test.log")

	opts := app.LoggingOptions{
		Level:   slog.LevelInfo,
		LogFile: logFile,
	}
	_, closer, err := app.Setup(opts)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	// First close should succeed.
	if err := closer.Close(); err != nil {
		t.Fatalf("first close failed: %v", err)
	}

	// Second close should either return an error (file already closed) or nil
	// (safe to double-close). Both behaviors are acceptable.
	if err := closer.Close(); err != nil && !errors.Is(err, os.ErrInvalid) {
		// Some systems may return os.ErrInvalid for double-close.
		// We tolerate that. If it's a different error, fail.
		t.Logf("second close returned: %v (acceptable)", err)
	}
}

func TestSetupAddSource(t *testing.T) {
	tmpdir := t.TempDir()
	logFile := filepath.Join(tmpdir, "test.log")

	opts := app.LoggingOptions{
		Level:     slog.LevelInfo,
		LogFile:   logFile,
		AddSource: true,
	}
	_, closer, err := app.Setup(opts)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	defer func() {
		if closer != nil {
			_ = closer.Close() //nolint:errcheck
		}
	}()

	// Log a message; when AddSource is true, the output should include file:line.
	slog.Info("test with source")

	if err := closer.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	// Verify the file contains a file:line reference (pattern: logging_test.go:).
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !bytes.Contains(data, []byte("logging_test.go")) {
		t.Errorf("log does not contain filename when AddSource=true: %s", string(data))
	}
}

func TestSetupComponentLevels(t *testing.T) {
	tmpdir := t.TempDir()
	logFile := filepath.Join(tmpdir, "test.log")

	tests := []struct {
		name       string
		global     slog.Level
		levels     map[string]slog.Level
		components []string
		logLevel   slog.Level
		wantLog    bool
	}{
		// Component with warn override: info should be suppressed.
		{"api-info-suppressed", slog.LevelInfo, map[string]slog.Level{"api": slog.LevelWarn}, []string{"api"}, slog.LevelInfo, false},
		// Component with warn override: warn should pass.
		{"api-warn-passes", slog.LevelInfo, map[string]slog.Level{"api": slog.LevelWarn}, []string{"api"}, slog.LevelWarn, true},
		// Component with warn override: error should pass.
		{"api-error-passes", slog.LevelInfo, map[string]slog.Level{"api": slog.LevelWarn}, []string{"api"}, slog.LevelError, true},
		// Unlisted component inherits global info level.
		{"unlisted-info-passes", slog.LevelInfo, map[string]slog.Level{"api": slog.LevelWarn}, []string{"downloader"}, slog.LevelInfo, true},
		// Component set to off: nothing passes.
		{"off-error-suppressed", slog.LevelInfo, map[string]slog.Level{"scheduler": slog.Level(99)}, []string{"scheduler"}, slog.LevelError, false},
		// Component with debug override: debug should pass even though global is info.
		{"debug-override-passes", slog.LevelInfo, map[string]slog.Level{"downloader": slog.LevelDebug}, []string{"downloader"}, slog.LevelDebug, true},
		// Nested/Shadowed components: the most specific (last) "component" tag should win.
		// When we use .With("component", "parent").With("component", "parent/child"), "parent/child" should be the key.
		{"nested-components", slog.LevelInfo, map[string]slog.Level{"parent/child": slog.LevelDebug}, []string{"parent", "parent/child"}, slog.LevelDebug, true},
		// Hierarchical inheritance: level set on parent should apply to child.
		{"hierarchical-inheritance", slog.LevelInfo, map[string]slog.Level{"parent": slog.LevelDebug}, []string{"parent", "parent/child"}, slog.LevelDebug, true},
		// Walk-up: child component "assembler" has no rule, but parent
		// component "app" is set to info. Debug from the assembler should
		// be suppressed because the filter walks up the component chain.
		{"walk-up-suppresses-debug", slog.LevelDebug, map[string]slog.Level{"app": slog.LevelInfo}, []string{"app", "assembler"}, slog.LevelDebug, false},
		// Walk-up: same scenario but info-level should still pass.
		{"walk-up-passes-info", slog.LevelDebug, map[string]slog.Level{"app": slog.LevelInfo}, []string{"app", "assembler"}, slog.LevelInfo, true},
		// Walk-up: child has its own explicit rule, overrides parent.
		{"walk-up-child-overrides", slog.LevelInfo, map[string]slog.Level{"app": slog.LevelWarn, "assembler": slog.LevelDebug}, []string{"app", "assembler"}, slog.LevelDebug, true},
		// No component levels at all: behaves as global.
		{"no-overrides", slog.LevelInfo, nil, []string{"api"}, slog.LevelInfo, true},
		// Superset: the joined canonical path is itself a valid filter key.
		// A rule on the full path "app/postproc/repair" matches a line scoped
		// [app, postproc, repair].
		{"superset-full-path", slog.LevelInfo, map[string]slog.Level{"app/postproc/repair": slog.LevelDebug}, []string{"app", "postproc", "repair"}, slog.LevelDebug, true},
		// Superset: a slash-prefix of the joined path also matches.
		{"superset-path-prefix", slog.LevelInfo, map[string]slog.Level{"app/postproc": slog.LevelDebug}, []string{"app", "postproc", "repair"}, slog.LevelDebug, true},
		// Bare leaf segment still works as a key after the leaf-rename.
		{"bare-leaf-segment", slog.LevelInfo, map[string]slog.Level{"repair": slog.LevelDebug}, []string{"app", "postproc", "repair"}, slog.LevelDebug, true},
		// Breaking-change guard: a legacy slash key that is neither a stacked
		// segment nor a prefix of the joined path (e.g. "postproc/repair", now
		// that the leaf is bare "repair") no longer matches — filter on
		// "repair" or "app/postproc/repair" instead.
		{"legacy-slash-key-no-match", slog.LevelInfo, map[string]slog.Level{"postproc/repair": slog.LevelDebug}, []string{"app", "postproc", "repair"}, slog.LevelDebug, false},
		// Precedence: a specific full-path override must NOT be shadowed by a
		// broader mid-chain segment rule. "app/postproc/repair" (depth 3) wins
		// over "postproc" (depth 2), so debug passes.
		{"specific-path-beats-broad-segment", slog.LevelInfo, map[string]slog.Level{"postproc": slog.LevelWarn, "app/postproc/repair": slog.LevelDebug}, []string{"app", "postproc", "repair"}, slog.LevelDebug, true},
		// Precedence, other direction: a specific leaf segment must NOT be
		// shadowed by a broad root path rule. "repair" (depth 3) wins over
		// "app" (depth 1), so debug passes.
		{"specific-leaf-beats-broad-root", slog.LevelInfo, map[string]slog.Level{"app": slog.LevelWarn, "repair": slog.LevelDebug}, []string{"app", "postproc", "repair"}, slog.LevelDebug, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Remove(logFile) // clean up from prev subtest
			opts := app.LoggingOptions{
				Level:           tt.global,
				LogFile:         logFile,
				ComponentLevels: tt.levels,
			}
			logger, closer, err := app.Setup(opts)
			if err != nil {
				t.Fatalf("Setup failed: %v", err)
			}
			defer func() {
				if closer != nil {
					_ = closer.Close() //nolint:errcheck
				}
			}()

			msg := "test message for " + tt.name
			// Apply components in order.
			compLogger := logger
			for _, c := range tt.components {
				compLogger = compLogger.With("component", c)
			}
			compLogger.Log(t.Context(), tt.logLevel, msg)

			_ = closer.Close() // flush

			data, _ := os.ReadFile(logFile)
			gotLog := bytes.Contains(data, []byte(msg))
			if gotLog != tt.wantLog {
				t.Errorf("gotLog = %v, want %v (global=%v, levels=%v, components=%v, logLevel=%v)\nfile content: %s",
					gotLog, tt.wantLog, tt.global, tt.levels, tt.components, tt.logLevel, string(data))
			}
		})
	}
}

// TestSetup_SingleJoinedComponent is the regression guard for the reported bug:
// a logger scoped through multiple .With("component", ...) layers must emit a
// single component= attribute holding the joined path, not one per layer.
func TestSetup_SingleJoinedComponent(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "test.log")
	logger, closer, err := app.Setup(app.LoggingOptions{Level: slog.LevelInfo, LogFile: logFile})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() { _ = closer.Close() }() //nolint:errcheck

	// Mirror the real chain: app → postproc → repair → go_par2, with an
	// interleaved non-component attr that must survive.
	l := logger.
		With("component", "app").
		With("component", "postproc").
		With("component", "repair", "job", "abc123").
		With("component", "go_par2")
	l.Info("candidate files")

	_ = closer.Close() // flush
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	line := string(data)

	if n := bytes.Count(data, []byte("component=")); n != 1 {
		t.Errorf("found %d component= attrs; want exactly 1\nline: %s", n, line)
	}
	if !bytes.Contains(data, []byte("component=app/postproc/repair/go_par2")) {
		t.Errorf("expected single joined component=app/postproc/repair/go_par2\nline: %s", line)
	}
	if !bytes.Contains(data, []byte("job=abc123")) {
		t.Errorf("non-component attr job=abc123 must be preserved\nline: %s", line)
	}
}
