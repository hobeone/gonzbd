package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

// TestBuildMetadataOnTheWire pins that every endpoint reporting build
// metadata carries the commit time and dirty flag from Options, alongside
// the pre-existing commit and build_date.
func TestBuildMetadataOnTheWire(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })

	tests := []struct {
		name  string
		opts  Options
		dirty bool
	}{
		{"clean", Options{Commit: "abc1234", CommitTime: "2026-05-01T10:00:00Z", Date: "2026-05-06T14:00:00Z"}, false},
		{"dirty", Options{Commit: "abc1234", CommitTime: "2026-05-01T10:00:00Z", Date: "2026-05-06T14:00:00Z", Dirty: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := tt.opts
			opts.Version = "v1.2.0"
			opts.Config = cfg
			s := New(opts)

			check := func(t *testing.T, m map[string]any) {
				t.Helper()
				if m["commit"] != "abc1234" {
					t.Errorf("commit = %v; want abc1234", m["commit"])
				}
				if m["commit_time"] != "2026-05-01T10:00:00Z" {
					t.Errorf("commit_time = %v; want 2026-05-01T10:00:00Z", m["commit_time"])
				}
				if m["dirty"] != tt.dirty {
					t.Errorf("dirty = %v; want %v", m["dirty"], tt.dirty)
				}
				if m["build_date"] != "2026-05-06T14:00:00Z" {
					t.Errorf("build_date = %v; want 2026-05-06T14:00:00Z", m["build_date"])
				}
			}

			t.Run("status build_info", func(t *testing.T) {
				rr := apiGet(t, s.Handler(), "/api?mode=status&name=build_info&apikey="+testAPIKey)
				if rr.Code != http.StatusOK {
					t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
				}
				check(t, decodeJSON(t, rr))
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
