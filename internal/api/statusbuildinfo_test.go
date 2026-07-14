package api

import (
	"net/http"
	"runtime/debug"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

func TestResolvedDependency(t *testing.T) {
	t.Parallel()

	t.Run("no replace", func(t *testing.T) {
		t.Parallel()
		got := resolvedDependency(&debug.Module{Path: "example.com/a", Version: "v1.0.0"})
		want := buildDependency{Path: "example.com/a", Version: "v1.0.0"}
		if got != want {
			t.Errorf("resolvedDependency() = %+v; want %+v", got, want)
		}
	})

	t.Run("replaced", func(t *testing.T) {
		t.Parallel()
		got := resolvedDependency(&debug.Module{
			Path:    "example.com/a",
			Version: "v1.0.0",
			Replace: &debug.Module{Path: "example.com/fork-of-a", Version: "v1.0.0-patched"},
		})
		want := buildDependency{Path: "example.com/fork-of-a", Version: "v1.0.0-patched"}
		if got != want {
			t.Errorf("resolvedDependency() = %+v; want %+v (replace target)", got, want)
		}
	})
}

func TestModeStatus_BuildInfo(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Version: "v1.2.0", Commit: "abc123", Date: "2026-07-14T00:00:00Z", Config: cfg})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=build_info&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	if m["version"] != "v1.2.0" {
		t.Errorf("version = %v; want v1.2.0", m["version"])
	}
	if m["commit"] != "abc123" {
		t.Errorf("commit = %v; want abc123", m["commit"])
	}
	if m["build_date"] != "2026-07-14T00:00:00Z" {
		t.Errorf("build_date = %v; want 2026-07-14T00:00:00Z", m["build_date"])
	}
	if m["go_version"] == nil || m["go_version"] == "" {
		t.Error("expected non-empty go_version")
	}

	// Running under `go test`, deps come from the test binary's own build
	// info rather than gonzbd's, so we only assert the field is present
	// and well-formed -- not that it's non-empty or contains a specific
	// module (that's exercised indirectly by every other test binary that
	// imports this module's dependencies).
	deps, ok := m["deps"].([]any)
	if !ok {
		t.Fatalf("expected deps array, got %v (%T)", m["deps"], m["deps"])
	}
	for _, d := range deps {
		dep, ok := d.(map[string]any)
		if !ok {
			t.Fatalf("expected dep object, got %v", d)
		}
		if dep["path"] == nil || dep["path"] == "" {
			t.Errorf("dep missing path: %v", dep)
		}
	}
}
