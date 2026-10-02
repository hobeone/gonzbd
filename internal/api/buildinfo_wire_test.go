package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hobeone/gonzbd/internal/buildinfo"
	"github.com/hobeone/gonzbd/internal/config"
)

// TestBuildMetadataOnTheWire pins that the three endpoints reporting build
// metadata carry the same five fields, and that an absent value is "" (false
// for dirty) rather than a placeholder. The three are the callers of
// writeBuildFields: `git grep -n 'writeBuildFields[(]' -- 'internal/api/*.go'
// ':(exclude)internal/api/*_test.go'` finds its definition plus one call in
// each of about.go, statusbuildinfo.go and statusoverview.go.
func TestBuildMetadataOnTheWire(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })

	tests := []struct {
		name  string
		build buildinfo.Info
	}{
		{"clean", buildinfo.Info{Version: "v1.2.0", Commit: "abc1234", CommitTime: "2026-05-01T10:00:00Z", BuildDate: "2026-05-06T14:00:00Z"}},
		{"dirty", buildinfo.Info{Version: "v1.2.0", Commit: "abc1234", CommitTime: "2026-05-01T10:00:00Z", BuildDate: "2026-05-06T14:00:00Z", Dirty: true}},
		{"absent", buildinfo.Info{Version: "dev"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := New(Options{Build: tt.build, Config: cfg})

			check := func(t *testing.T, m map[string]any) {
				t.Helper()
				want := map[string]any{
					"version":     tt.build.Version,
					"commit":      tt.build.Commit,
					"commit_time": tt.build.CommitTime,
					"dirty":       tt.build.Dirty,
					"build_date":  tt.build.BuildDate,
				}
				for k, v := range want {
					if m[k] != v {
						t.Errorf("%s = %#v; want %#v", k, m[k], v)
					}
				}
			}

			t.Run("status build_info", func(t *testing.T) {
				rr := apiGet(t, s.Handler(), "/api?mode=status&name=build_info&apikey="+testAPIKey)
				if rr.Code != http.StatusOK {
					t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
				}
				check(t, decodeJSON(t, rr))
			})
			t.Run("status_overview general", func(t *testing.T) {
				rr := apiGet(t, s.Handler(), "/api?mode=status_overview&apikey="+testAPIKey)
				if rr.Code != http.StatusOK {
					t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
				}
				general, ok := decodeJSON(t, rr)["general"].(map[string]any)
				if !ok {
					t.Fatalf("no general object in %s", rr.Body.String())
				}
				check(t, general)
			})
			t.Run("about", func(t *testing.T) {
				// A cancelled request context makes the handler's outbound
				// public-IP lookups fail immediately instead of dialling out.
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				rr := httptest.NewRecorder()
				s.modeAbout(rr, httptest.NewRequest(http.MethodGet, "/api?mode=about", nil).WithContext(ctx))
				about, ok := decodeJSON(t, rr)["about"].(map[string]any)
				if !ok {
					t.Fatalf("no about object in %s", rr.Body.String())
				}
				check(t, about)
			})
		})
	}
}
