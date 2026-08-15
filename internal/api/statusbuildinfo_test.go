package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
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

// TestStatusBuildInfo_ReportsTheBinaryItIsRunningIn pins the handler's
// dependency list against the process it is actually running in.
//
// TestModeStatus_BuildInfo above covers the route and the scalar fields, and
// deliberately asserts only that each dep has a non-empty path — its comment
// explains why it does not go further. This goes further, and can: the point of
// the endpoint is that it reports what was COMPILED IN rather than what go.mod
// says, and debug.ReadBuildInfo() is available inside a test binary, so the
// handler's list can be compared element-for-element against the runtime's own
// view. A test that fed it a hand-built list would assert the opposite of the
// endpoint's purpose.
func TestStatusBuildInfo_ReportsTheBinaryItIsRunningIn(t *testing.T) {
	t.Parallel()
	s := &Server{version: "1.2.3", commit: "abc1234", date: "2026-08-12"}

	rec := httptest.NewRecorder()
	s.statusBuildInfo(rec, httptest.NewRequest(http.MethodGet, "/api/status/buildinfo", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got struct {
		Status    bool              `json:"status"`
		Version   string            `json:"version"`
		Commit    string            `json:"commit"`
		BuildDate string            `json:"build_date"`
		GoVersion string            `json:"go_version"`
		Deps      []buildDependency `json:"deps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, rec.Body.String())
	}

	if !got.Status {
		t.Error("status = false, want true")
	}
	if got.Version != "1.2.3" || got.Commit != "abc1234" || got.BuildDate != "2026-08-12" {
		t.Errorf("version/commit/build_date = %q/%q/%q, want the Server's own fields",
			got.Version, got.Commit, got.BuildDate)
	}
	if got.GoVersion != runtime.Version() {
		t.Errorf("go_version = %q, want %q", got.GoVersion, runtime.Version())
	}

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build info in this test binary; the dependency half cannot be checked")
	}
	if len(got.Deps) != len(bi.Deps) {
		t.Fatalf("deps: got %d, want %d — one entry per compiled-in module",
			len(got.Deps), len(bi.Deps))
	}
	for i, m := range bi.Deps {
		want := resolvedDependency(m)
		if got.Deps[i] != want {
			t.Errorf("deps[%d] = %+v, want %+v (replace directives must be resolved)", i, got.Deps[i], want)
		}
	}
}

// TestStatusBuildInfo_DepsIsAnArrayNotNull pins the empty-slice initialisation.
//
// `deps := []buildDependency{}` rather than a nil slice is what makes the field
// marshal as `[]`. A nil slice marshals as `null`, which a JS client indexing
// the response would throw on rather than render as an empty list — a real
// difference in the wire contract, and one no other assertion here would catch
// because this process always has dependencies.
func TestStatusBuildInfo_DepsIsAnArrayNotNull(t *testing.T) {
	t.Parallel()
	var raw map[string]json.RawMessage
	rec := httptest.NewRecorder()
	(&Server{}).statusBuildInfo(rec, httptest.NewRequest(http.MethodGet, "/api/status/buildinfo", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["deps"]) == "null" {
		t.Error("deps marshalled as null; it must be an array, so the handler has to " +
			"initialise the slice rather than declare it")
	}
}
