package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
)

// sonarrVersionRegex is Sonarr's own parser, copied verbatim from
// Sonarr/src/NzbDrone.Core/Download/Clients/Sabnzbd/Sabnzbd.cs:38 (and the
// byte-identical line in Radarr's copy):
//
//	new Regex(@"(?<major>\d+)\.(?<minor>\d+)\.(?<patch>\d+|x)")
//
// It is unanchored and Sonarr takes the first match, so leading or trailing
// text is tolerated. A string it fails to match is reported to the user as
// "Unknown Version: <raw>".
var sonarrVersionRegex = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+|x)`)

// buildVersions are the build-version strings that must not reach a SABnzbd
// API client. "dev" is cmd/gonzbd/main.go's local-build default and is what
// Sonarr rejected in the field ("Unknown Version: dev"); it matches no digits
// and is not the literal "develop" that Sonarr special-cases to 3.0.0.
// "v0.1.0" is the near miss — it parses, but as major 0 it fails Sonarr's
// gates just as completely, which is why tagging a release is not the fix.
var buildVersions = []string{"dev", "v0.1.0", "1.0.0-test", ""}

// TestModeVersion_SatisfiesSonarrVersionGate pins the client contract rather
// than the constant: mode=version must parse under Sonarr's regex and clear
// its version gates, whatever gonzbd's own build version happens to be.
// Asserting only that the body equals sabnzbdAPIVersion would be a change
// detector — it would still pass if the constant were pointed back at
// s.version.
func TestModeVersion_SatisfiesSonarrVersionGate(t *testing.T) {
	t.Parallel()
	for _, bv := range buildVersions {
		t.Run("build="+bv, func(t *testing.T) {
			t.Parallel()
			s := New(Options{Version: bv})
			rr := apiGet(t, s.Handler(), "/api?mode=version")
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200", rr.Code)
			}
			raw, ok := decodeJSON(t, rr)["version"].(string)
			if !ok {
				t.Fatalf("version is not a string")
			}

			m := sonarrVersionRegex.FindStringSubmatch(raw)
			if m == nil {
				t.Fatalf("version %q does not match Sonarr's VersionRegex; "+
					"Sonarr reports this as `Unknown Version: %s`", raw, raw)
			}
			major, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("major %q: %v", m[1], err)
			}

			// Sonarr gates GetCategories on HasVersion(2, 0): below 2.0 it
			// abandons mode=fullstatus and reads default_root_folder off
			// mode=queue, which gonzbd does not emit
			// (`git grep -n 'root_folder["]' -- '*.go'` finds 0 — the bracket
			// keeps the pattern from matching its own comment). Major >= 2 also
			// subsumes the weaker `Major >= 1 || Minor >= 7` check that
			// TestConnectionAndVersion applies.
			if major < 2 {
				t.Errorf("version %q parses as major %d; want >= 2 so Sonarr "+
					"takes the mode=fullstatus path for a relative "+
					"complete_dir", raw, major)
			}
		})
	}
}

// TestModeVersion_DecoupledFromBuildVersion pins the split that makes the gate
// above stable: mode=version reports the SABnzbd API generation gonzbd
// implements, not its own build version. Coupling them is what let the "dev"
// default reach Sonarr, and would break clients again on any pre-1.0 tag.
func TestModeVersion_DecoupledFromBuildVersion(t *testing.T) {
	t.Parallel()
	for _, bv := range buildVersions {
		t.Run("build="+bv, func(t *testing.T) {
			t.Parallel()
			s := New(Options{Version: bv})
			rr := apiGet(t, s.Handler(), "/api?mode=version")
			if got := decodeJSON(t, rr)["version"]; got == bv {
				t.Errorf("version = %q, which is the build version; "+
					"mode=version must report the SABnzbd API generation", got)
			}
		})
	}
}

// TestModeVersion_IgnoresRequest calls the handler directly, bypassing the
// router, to pin that its answer is a property of the build and not of the
// request: no parameter, header, method or credential changes it. That is what
// makes the constant safe to feature-gate on — a client polling mode=version
// twice cannot get two different answers.
func TestModeVersion_IgnoresRequest(t *testing.T) {
	t.Parallel()
	s := New(Options{Version: "dev"})
	reqs := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api?mode=version", nil),
		httptest.NewRequest(http.MethodGet, "/api?mode=version&version=9.9.9", nil),
		httptest.NewRequest(http.MethodPost, "/api?mode=version&apikey="+testAPIKey, nil),
	}
	for i, req := range reqs {
		rr := httptest.NewRecorder()
		s.modeVersion(rr, req)
		var m map[string]any
		if err := json.NewDecoder(rr.Body).Decode(&m); err != nil {
			t.Fatalf("req %d: decode: %v", i, err)
		}
		if m["version"] != sabnzbdAPIVersion {
			t.Errorf("req %d (%s %s): version = %v; want %v",
				i, req.Method, req.URL, m["version"], sabnzbdAPIVersion)
		}
	}
}
