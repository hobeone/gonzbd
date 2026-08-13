package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"testing"
)

// TestStatusBuildInfo_ReportsTheBinaryItIsRunningIn pins the handler against
// the process it is actually running in, rather than against a fixture.
//
// The point of the endpoint is that it reports what was COMPILED IN, not what
// go.mod says, so a test that fed it a hand-built dependency list would assert
// the opposite of its purpose. debug.ReadBuildInfo() is available inside a test
// binary, so the assertion here is that the handler's output agrees with the
// runtime's own view.
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

// TestResolvedDependency_FollowsAReplaceDirective is the unit behind the
// dependency half above. The replace case cannot be produced from this test
// binary's own build info — nothing in go.mod is replaced — so it is
// constructed directly.
func TestResolvedDependency_FollowsAReplaceDirective(t *testing.T) {
	t.Parallel()
	plain := &debug.Module{Path: "example.com/a", Version: "v1.0.0"}
	if got := resolvedDependency(plain); got != (buildDependency{Path: "example.com/a", Version: "v1.0.0"}) {
		t.Errorf("unreplaced module: got %+v", got)
	}

	replaced := &debug.Module{
		Path:    "example.com/a",
		Version: "v1.0.0",
		Replace: &debug.Module{Path: "example.com/fork", Version: "v0.9.9"},
	}
	got := resolvedDependency(replaced)
	if got != (buildDependency{Path: "example.com/fork", Version: "v0.9.9"}) {
		t.Errorf("replaced module: got %+v, want the replacement's path and version — "+
			"reporting the original would name a module that is not in the binary", got)
	}
}
