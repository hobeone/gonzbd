package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecode_ErrorShowsSourceContext(t *testing.T) {
	t.Parallel()

	const malformed = "general:\n  host: [unclosed list\n"
	_, _, err := decode(strings.NewReader(malformed))
	if err == nil {
		t.Fatal("decode: expected a YAML syntax error, got nil")
	}
	if !strings.Contains(err.Error(), "Context:") {
		t.Errorf("error should contain a %q section, got:\n%v", "Context:", err)
	}
	if !strings.Contains(err.Error(), "host: [unclosed list") {
		t.Errorf("error should quote the offending line, got:\n%v", err)
	}
}

// ---------- decode sticky defaults ----------

// TestDecode_StickyReplaceIllegalWith verifies that setting replace_illegal_with
// to empty in YAML is overridden by the sticky default "_". An empty value would
// produce filenames with illegal characters on Windows paths.
func TestDecode_StickyReplaceIllegalWith(t *testing.T) {
	t.Parallel()

	// Replace the existing value in the full config YAML; appending a
	// duplicate key would be a fatal YAML error.
	yaml := strings.ReplaceAll(minimalYAML(t), "replace_illegal_with: _", "replace_illegal_with: \"\"")
	cfg, _, err := decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Downloads.ReplaceIllegalWith != "_" {
		t.Errorf("ReplaceIllegalWith = %q, want \"_\" (sticky default)", cfg.Downloads.ReplaceIllegalWith)
	}
}

// TestDecode_StickyCleanupList verifies that setting cleanup_list to an empty
// sequence in YAML is overridden by DefaultCleanupList. An empty cleanup list
// would break job-name sanitization for all users on upgrade.
func TestDecode_StickyCleanupList(t *testing.T) {
	t.Parallel()

	// Blank out the cleanup_list block. The YAML from Save() serializes it
	// as a multi-line sequence; replace the first entry's prefix so the
	// unmarshal produces an empty slice and the sticky default kicks in.
	base := minimalYAML(t)
	// Remove all cleanup_list entries by replacing the field with an empty sequence.
	lines := strings.Split(base, "\n")
	var out []string
	inCleanup := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "cleanup_list:") {
			out = append(out, "  cleanup_list: []")
			inCleanup = true
			continue
		}
		if inCleanup {
			if strings.HasPrefix(trimmed, "-") {
				continue // skip list items
			}
			inCleanup = false
		}
		out = append(out, line)
	}
	yaml := strings.Join(out, "\n")

	cfg, _, err := decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.Downloads.CleanupList) == 0 {
		t.Error("CleanupList is empty; want DefaultCleanupList (sticky default)")
	}
}

// ---------- nil-to-empty normalization ----------

// TestDecode_NilServersNormalized verifies that an explicit "servers: null"
// (or omitted servers key) produces an empty (non-nil) slice so JSON
// serialization returns [] not null.
func TestDecode_NilServersNormalized(t *testing.T) {
	t.Parallel()

	// Replace the servers block with an explicit null to exercise the
	// nil-normalization code path.
	base := minimalYAML(t)
	lines := strings.Split(base, "\n")
	var out []string
	inServers := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "servers:") {
			out = append(out, "servers: null")
			inServers = true
			continue
		}
		if inServers {
			// Skip continuation lines that belong to the servers block.
			if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") {
				continue
			}
			inServers = false
		}
		out = append(out, line)
	}

	cfg, _, err := decode(strings.NewReader(strings.Join(out, "\n")))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Servers == nil {
		t.Error("Servers = nil; want []ServerConfig{} (empty, not nil)")
	}
}

// ---------- Load error paths ----------

// TestLoad_MissingFile verifies that Load returns an error containing the path
// when the config file does not exist.
func TestLoad_MissingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nonexistent.yaml")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load: expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not mention path %q", err.Error(), path)
	}
}

// TestLoad_ValidationFailure verifies that Load returns an error when the
// file parses correctly but fails Validate() (e.g. missing required port).
func TestLoad_ValidationFailure(t *testing.T) {
	t.Parallel()

	// Write a minimal config that is syntactically valid but fails validation
	// (port=0 is not allowed).
	yaml := minimalYAML(t)
	yaml = strings.ReplaceAll(yaml, "port: 4289", "port: 0")

	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load: expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "validate") {
		t.Errorf("error %q does not mention validate", err.Error())
	}
}

// ---------- decode type errors ----------

// TestDecode_FatalTypeError verifies that a real type mismatch (e.g. string
// where int is required) is returned as a fatal error, not silently swallowed.
func TestDecode_FatalTypeError(t *testing.T) {
	t.Parallel()

	yaml := minimalYAML(t) + "\ngeneral:\n  port: \"not-a-number\"\n"
	_, _, err := decode(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("decode: expected error for type mismatch, got nil")
	}
}

// ---------- server port normalization ----------

// withServersYAML replaces the empty "servers: []" key that minimalYAML's
// base config renders with the given servers block. minimalYAML's Default()
// config already emits an explicit (empty) "servers:" key, so the plan's
// originally-assumed append-only fixture would produce a duplicate "servers"
// mapping key and fail to parse; replace instead of append.
func withServersYAML(t *testing.T, serversBlock string) string {
	t.Helper()
	base := minimalYAML(t)
	yaml := strings.Replace(base, "servers: []\n", serversBlock, 1)
	if yaml == base {
		t.Fatal("withServersYAML: \"servers: []\" not found in minimalYAML base; fixture assumption is stale")
	}
	return yaml
}

func TestDecode_ServerPortDefaultsPlain(t *testing.T) {
	t.Parallel()
	yaml := withServersYAML(t, "servers:\n  - name: s1\n    host: news.example.com\n    connections: 1\n")
	cfg, _, err := decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(cfg.Servers))
	}
	if cfg.Servers[0].Port != 119 {
		t.Errorf("Port = %d, want 119 (plain default)", cfg.Servers[0].Port)
	}
}

func TestDecode_ServerPortDefaultsSSL(t *testing.T) {
	t.Parallel()
	yaml := withServersYAML(t, "servers:\n  - name: s1\n    host: news.example.com\n    connections: 1\n    ssl: true\n")
	cfg, _, err := decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(cfg.Servers))
	}
	if cfg.Servers[0].Port != 563 {
		t.Errorf("Port = %d, want 563 (ssl default)", cfg.Servers[0].Port)
	}
}

func TestDecode_ServerPortExplicitNotOverridden(t *testing.T) {
	t.Parallel()
	yaml := withServersYAML(t, "servers:\n  - name: s1\n    host: news.example.com\n    connections: 1\n    port: 8119\n")
	cfg, _, err := decode(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Servers[0].Port != 8119 {
		t.Errorf("Port = %d, want 8119 (explicit value preserved)", cfg.Servers[0].Port)
	}
}

// ---------- helper ----------

// minimalYAML returns a YAML string that passes Validate() and is suitable
// as a base for decode tests.
func minimalYAML(t *testing.T) string {
	t.Helper()
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	// Render to YAML via Save/re-read round-trip so we always have a
	// syntactically and semantically valid base.
	dir := t.TempDir()
	path := filepath.Join(dir, "base.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestLoaderUnexportedHelpersDirect(t *testing.T) {
	t.Parallel()

	t.Run("partitionYAMLErrors", func(t *testing.T) {
		warns, err := partitionYAMLErrors(nil)
		if len(warns) != 0 || err != nil {
			t.Errorf("expected (nil, nil) for nil, got %v, %v", warns, err)
		}

		stdErr := fmt.Errorf("some standard error")
		warns, err = partitionYAMLErrors(stdErr)
		if len(warns) != 0 || !errors.Is(err, stdErr) {
			t.Errorf("expected stdErr returned as-is, got %v, %v", warns, err)
		}
	})

	t.Run("applyNormalization", func(t *testing.T) {
		cfg := &Config{}
		cfg.applyNormalization()

		if cfg.Downloads.ReplaceIllegalWith != "_" {
			t.Errorf("ReplaceIllegalWith: got %q, want '_'", cfg.Downloads.ReplaceIllegalWith)
		}
		if len(cfg.Downloads.CleanupList) == 0 {
			t.Errorf("CleanupList was not populated with sticky defaults")
		}
		if cfg.Servers == nil {
			t.Error("Servers is nil, expected []")
		}
		if cfg.Categories == nil {
			t.Error("Categories is nil, expected []")
		}
	})
}

type testLogRecorder struct {
	attrs   []slog.Attr
	records *[]slog.Record
}

func (h *testLogRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *testLogRecorder) Handle(_ context.Context, r slog.Record) error {
	for _, a := range h.attrs {
		r.AddAttrs(a)
	}
	*h.records = append(*h.records, r)
	return nil
}
func (h *testLogRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &testLogRecorder{attrs: newAttrs, records: h.records}
}
func (h *testLogRecorder) WithGroup(string) slog.Handler { return h }

func TestLoad_ComponentScopedLogging(t *testing.T) {
	oldLogger := slog.Default()
	records := &[]slog.Record{}
	slog.SetDefault(slog.New(&testLogRecorder{records: records}))
	defer slog.SetDefault(oldLogger)

	dir := t.TempDir()
	path := filepath.Join(dir, "test_config.yaml")
	yaml := minimalYAML(t) + "\nunknown_test_field: 123\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(*records) == 0 {
		t.Fatal("expected at least one log record for unknown field, got 0")
	}

	foundComponent := false
	for _, rec := range *records {
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "component" && a.Value.String() == "config" {
				foundComponent = true
				return false
			}
			return true
		})
	}
	if !foundComponent {
		t.Errorf("expected warning log record to have attribute component=config; records: %v", *records)
	}
}
