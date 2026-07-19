package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// Malformed version components parse as 0, avoiding panic and degrading gracefully.
	if got := compareVersions("not-a-version", "v1.2.0"); got != -1 {
		t.Errorf("compareVersions('not-a-version', 'v1.2.0') = %d, want -1", got)
	}
	if got := compareVersions("v1.2.0", "also-not-a-version"); got != 1 {
		t.Errorf("compareVersions('v1.2.0', 'also-not-a-version') = %d, want 1", got)
	}
	if got := compareVersions("garbage1", "garbage2"); got != 0 {
		t.Errorf("compareVersions('garbage1', 'garbage2') = %d, want 0", got)
	}
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
	reason, _ := result["reason"].(string)
	if !strings.Contains(reason, "no version baked in") {
		t.Errorf("result.reason = %q; want it to explain the dev-build short circuit", reason)
	}
}

// TestUpdateCheckFailureReason exercises the reason-classification logic
// directly, since the HTTP-level tests can only easily construct
// HTTP-status and malformed-body failures via httptest.Server -- not a
// genuine network-unreachable or context-deadline-exceeded error.
func TestUpdateCheckFailureReason(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantSub string
	}{
		{"404 not found", &githubReleaseHTTPError{StatusCode: http.StatusNotFound}, "no GitHub release has been published"},
		{"500 server error", &githubReleaseHTTPError{StatusCode: http.StatusInternalServerError}, "unexpected status (500)"},
		{"403 rate limited", &githubReleaseHTTPError{StatusCode: http.StatusForbidden}, "unexpected status (403)"},
		{"empty tag name", errEmptyTagName, "missing a release tag name"},
		{"deadline exceeded", context.DeadlineExceeded, "did not respond in time"},
		{"generic network error", errors.New("dial tcp: lookup api.github.com: no such host"), "could not reach GitHub"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := updateCheckFailureReason(tc.err)
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("updateCheckFailureReason(%v) = %q; want substring %q", tc.err, got, tc.wantSub)
			}
		})
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
		wantReasonSub  string
	}{
		{"up to date", "v1.2.0", "v1.2.0", http.StatusOK, "up_to_date", "v1.2.0", ""},
		{"update available", "v1.2.0", "v1.3.0", http.StatusOK, "update_available", "v1.3.0", ""},
		{"no release published", "v1.2.0", "", http.StatusNotFound, "unknown", "", "no GitHub release has been published"},
		{"github error status", "v1.2.0", "", http.StatusInternalServerError, "unknown", "", "unexpected status (500)"},
		{"empty tag_name", "v1.2.0", "", http.StatusOK, "unknown", "", "missing a release tag name"},
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
			if tc.wantReasonSub != "" {
				reason, _ := result["reason"].(string)
				if !strings.Contains(reason, tc.wantReasonSub) {
					t.Errorf("result.reason = %q; want substring %q", reason, tc.wantReasonSub)
				}
			}
		})
	}
}
