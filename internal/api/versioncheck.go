package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// checkUpdateTimeout bounds the GitHub API call so an unreachable or
// slow GitHub never blocks the caller for long. This handler is
// deliberately separate from mode=status_overview and fetched
// independently by the UI so it can't add latency to the rest of the
// status page.
const checkUpdateTimeout = 3 * time.Second

// githubLatestReleaseURL is a var, not a const, specifically so tests can
// point it at a local httptest.Server instead of the real GitHub API —
// the same test-seam pattern already used elsewhere in this codebase
// (e.g. internal/cmdutil's `var lookPath = exec.LookPath`). Without this,
// statusCheckUpdate's status-mapping logic (up_to_date/update_available)
// would be untestable without hitting the real network.
var githubLatestReleaseURL = "https://api.github.com/repos/hobeone/gonzbd/releases/latest"

// statusCheckUpdate compares the running build's version against the
// latest GitHub release. Reports status "unknown" (never an HTTP error)
// on any failure: dev build, network error, timeout, or non-2xx
// response — this is informational only, never load-bearing.
func (s *Server) statusCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if s.version == "" || s.version == "dev" {
		respondOK(w, "result", map[string]any{
			"status": "unknown",
			"reason": "this build has no version baked in (a local build without -ldflags -X main.Version=...)",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), checkUpdateTimeout)
	defer cancel()

	latest, err := fetchLatestGithubRelease(ctx, "gonzbd/"+s.version)
	if err != nil {
		s.log.Debug("check_update: github fetch failed", "error", err)
		respondOK(w, "result", map[string]any{
			"status": "unknown",
			"reason": updateCheckFailureReason(err),
		})
		return
	}

	cmp := compareVersions(s.version, latest)
	status := "up_to_date"
	if cmp < 0 {
		status = "update_available"
	}
	respondOK(w, "result", map[string]any{"status": status, "latest_version": latest})
}

// githubReleaseHTTPError is a typed error carrying the GitHub API's HTTP
// status code, so updateCheckFailureReason can give a specific, readable
// explanation (e.g. distinguishing "no release published yet" from
// "rate limited") instead of a generic network failure message.
type githubReleaseHTTPError struct {
	StatusCode int
}

func (e *githubReleaseHTTPError) Error() string {
	return fmt.Sprintf("github releases API: status %d", e.StatusCode)
}

// updateCheckFailureReason turns a fetchLatestGithubRelease error into a
// short, human-readable explanation for the status page's Update field.
// The raw error (a dial/DNS message, a wrapped context error, etc.) is not
// shown directly -- it's logged at Debug for troubleshooting, but not
// useful to a user glancing at the status page.
func updateCheckFailureReason(err error) string {
	if httpErr, ok := errors.AsType[*githubReleaseHTTPError](err); ok {
		if httpErr.StatusCode == http.StatusNotFound {
			return "no GitHub release has been published for this repository yet"
		}
		return fmt.Sprintf("GitHub API returned an unexpected status (%d), possibly rate-limited", httpErr.StatusCode)
	}
	if errors.Is(err, errEmptyTagName) {
		return "GitHub's response was missing a release tag name (unexpected API response)"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "GitHub did not respond in time (check network/firewall access to api.github.com)"
	}
	return "could not reach GitHub (check network/firewall access to api.github.com)"
}

// errEmptyTagName is returned when GitHub's release API responds 200 OK
// but the body has no tag_name -- a malformed/unexpected response
// distinct from "no release published" (which is a 404, see
// githubReleaseHTTPError).
var errEmptyTagName = errors.New("github release response missing tag_name")

// fetchLatestGithubRelease queries the GitHub releases API for this
// repo's latest release tag. No caching: this only fires when a human
// opens/refreshes the status page, so GitHub's unauthenticated rate
// limit (60 req/hr/IP) is not a practical concern.
func fetchLatestGithubRelease(ctx context.Context, userAgent string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubLatestReleaseURL, http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort cleanup

	if resp.StatusCode != http.StatusOK {
		return "", &githubReleaseHTTPError{StatusCode: resp.StatusCode}
	}

	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.TagName == "" {
		return "", errEmptyTagName
	}
	return body.TagName, nil
}

// compareVersions compares two "vMAJOR.MINOR.PATCH"-style version
// strings numerically (not lexicographically — "v1.9.0" < "v1.10.0").
// Returns -1 if a < b, 0 if equal, 1 if a > b. Malformed components
// parse as 0, so garbage input degrades gracefully rather than
// panicking (this is informational-only display logic, not a security
// or correctness boundary).
func compareVersions(a, b string) int {
	pa := parseVersionParts(a)
	pb := parseVersionParts(b)
	for i := range 3 {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersionParts(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if idx := strings.IndexAny(v, "-+"); idx != -1 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	var out [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		n, _ := strconv.Atoi(parts[i]) //nolint:errcheck // malformed input defaults to 0, see doc comment
		out[i] = n
	}
	return out
}
