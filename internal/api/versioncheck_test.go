package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		current, latest string
		want            int // -1 current<latest, 0 equal, 1 current>latest
	}{
		{"v1.2.0", "v1.2.0", 0},
		{"v1.2.0", "v1.3.0", -1},
		{"v1.2.0", "v1.1.9", 1},
		{"v1.2.0", "v2.0.0", -1},
		{"v1.9.0", "v1.10.0", -1},        // numeric, not lexicographic, comparison
		{"v1.2", "v1.2.0", 0},            // missing patch defaults to 0
		{"v1.2.1-2b7f9150", "v1.2.1", 0}, // strips build metadata
		{"v1.2.0-rc1", "v1.2.0", 0},      // strips prerelease suffixes
		{"v1.2.3.4", "v1.2.3", 0},        // extra 4th component is ignored, not folded into patch
	}
	for _, tc := range tests {
		t.Run(tc.current+"_vs_"+tc.latest, func(t *testing.T) {
			got := compareVersions(tc.current, tc.latest)
			if got != tc.want {
				t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.current, tc.latest, got, tc.want)
			}
		})
	}
}

func TestCompareVersions_InvalidInput(t *testing.T) {
	// Malformed versions must not panic; exact return value for garbage
	// input is not asserted beyond "does not panic."
	_ = compareVersions("not-a-version", "v1.2.0")
	_ = compareVersions("v1.2.0", "also-not-a-version")
}

// TestModeStatus_CheckUpdate_DevBuildSkipsNetworkCall proves the dev-build
// short circuit actually skips the GitHub call, rather than merely
// producing the same "unknown" status a failed network call would also
// produce. It points githubLatestReleaseURL at an httptest.Server whose
// handler fails the test if it is ever invoked, so the assertion below
// discriminates "short-circuited before any call" from "attempted and
// failed" (which also yields status "unknown", see
// TestModeStatus_CheckUpdate_StatusMapping's "github error status" case).
func TestModeStatus_CheckUpdate_DevBuildSkipsNetworkCall(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("githubLatestReleaseURL should not be called for a dev build")
	}))
	defer github.Close()

	origURL := githubLatestReleaseURL
	githubLatestReleaseURL = github.URL
	defer func() { githubLatestReleaseURL = origURL }()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Version: "dev", Config: cfg})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=check_update&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result := m["result"].(map[string]any)
	if result["status"] != "unknown" {
		t.Errorf("result.status = %v; want unknown for a dev build", result["status"])
	}
}

// TestModeStatus_CheckUpdate_StatusMapping exercises statusCheckUpdate's
// full status-mapping logic (up_to_date / update_available / unknown on
// error) end-to-end via HTTP, using an httptest.Server in place of the
// real GitHub API by overriding the githubLatestReleaseURL var. Without
// this, only compareVersions itself (a pure function) would be tested,
// leaving the 3-line mapping in statusCheckUpdate uncovered.
func TestModeStatus_CheckUpdate_StatusMapping(t *testing.T) {
	tests := []struct {
		name           string
		runningVersion string
		latestTag      string
		serverStatus   int
		wantStatus     string
		wantLatest     string
	}{
		{"up to date", "v1.2.0", "v1.2.0", http.StatusOK, "up_to_date", "v1.2.0"},
		{"update available", "v1.2.0", "v1.3.0", http.StatusOK, "update_available", "v1.3.0"},
		{"github error status", "v1.2.0", "", http.StatusInternalServerError, "unknown", ""},
		{"empty tag_name", "v1.2.0", "", http.StatusOK, "unknown", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.serverStatus)
				if tc.serverStatus == http.StatusOK {
					_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tc.latestTag})
				}
			}))
			defer github.Close()

			origURL := githubLatestReleaseURL
			githubLatestReleaseURL = github.URL
			defer func() { githubLatestReleaseURL = origURL }()

			cfg, err := config.Default()
			if err != nil {
				t.Fatalf("Default(): %v", err)
			}
			cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
			s := New(Options{Version: tc.runningVersion, Config: cfg})

			rr := apiGet(t, s.Handler(), "/api?mode=status&name=check_update&apikey="+testAPIKey)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
			}
			m := decodeJSON(t, rr)
			result := m["result"].(map[string]any)
			if result["status"] != tc.wantStatus {
				t.Errorf("result.status = %v; want %v", result["status"], tc.wantStatus)
			}
			if tc.wantLatest != "" && result["latest_version"] != tc.wantLatest {
				t.Errorf("result.latest_version = %v; want %v", result["latest_version"], tc.wantLatest)
			}
		})
	}
}
