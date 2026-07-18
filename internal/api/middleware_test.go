package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

// ---------- isCrossOrigin ----------

func TestIsCrossOrigin_NoHeaders(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "localhost:4289"
	if isCrossOrigin(r) {
		t.Error("no headers should not be cross-origin")
	}
}

func TestIsCrossOrigin_SameOrigin(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "localhost:4289"
	r.Header.Set("Origin", "http://localhost:4289")
	if isCrossOrigin(r) {
		t.Error("same-host Origin should not be cross-origin")
	}
}

func TestIsCrossOrigin_LoopbackOrigin(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "localhost:4289"
	r.Header.Set("Origin", "http://127.0.0.1:4289")
	if isCrossOrigin(r) {
		t.Error("loopback IP origin should not be cross-origin")
	}
}

func TestIsCrossOrigin_LocalhostName(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "127.0.0.1:4289"
	r.Header.Set("Origin", "http://localhost:9090")
	if isCrossOrigin(r) {
		t.Error("localhost name origin should not be cross-origin")
	}
}

func TestIsCrossOrigin_ExternalOrigin(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "localhost:4289"
	r.Header.Set("Origin", "http://evil.example.com")
	if !isCrossOrigin(r) {
		t.Error("external origin MUST be cross-origin")
	}
}

func TestIsCrossOrigin_MalformedOrigin(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "localhost:4289"
	r.Header.Set("Origin", "://bad")
	if !isCrossOrigin(r) {
		t.Error("malformed origin should be treated as cross-origin")
	}
}

func TestIsCrossOrigin_SecFetchSiteCrossSite(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "localhost:4289"
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if !isCrossOrigin(r) {
		t.Error("Sec-Fetch-Site: cross-site MUST be cross-origin")
	}
}

func TestIsCrossOrigin_SecFetchSiteCrossOrigin(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "localhost:4289"
	r.Header.Set("Sec-Fetch-Site", "cross-origin")
	if !isCrossOrigin(r) {
		t.Error("Sec-Fetch-Site: cross-origin MUST be cross-origin")
	}
}

func TestIsCrossOrigin_SecFetchSiteSameOrigin(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "localhost:4289"
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if isCrossOrigin(r) {
		t.Error("Sec-Fetch-Site: same-origin should not be cross-origin")
	}
}

func TestIsCrossOrigin_RefererCrossOrigin(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "localhost:4289"
	r.Header.Set("Referer", "http://evil.com/page")
	if !isCrossOrigin(r) {
		t.Error("non-local Referer MUST be cross-origin")
	}
}

func TestIsCrossOrigin_RefererSameHost(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "localhost:4289"
	r.Header.Set("Referer", "http://localhost:4289/config")
	if isCrossOrigin(r) {
		t.Error("same-host Referer should not be cross-origin")
	}
}

func TestIsCrossOrigin_RefererLoopback(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Host = "localhost:4289"
	r.Header.Set("Referer", "http://127.0.0.1:4289/page")
	if isCrossOrigin(r) {
		t.Error("loopback Referer should not be cross-origin")
	}
}

func TestIsRefererCrossOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		referer     string
		currentHost string
		want        bool
	}{
		{"empty referer", "", "localhost:4289", false},
		{"malformed referer", "://bad", "localhost:4289", false},
		{"same host referer", "http://localhost:4289/config", "localhost:4289", false},
		{"different host referer", "http://evil.com/page", "localhost:4289", true},
		{"loopback IP referer", "http://127.0.0.1:4289/page", "localhost:4289", false},
		{"localhost name referer", "http://localhost:9090/page", "127.0.0.1:4289", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isRefererCrossOrigin(tc.referer, tc.currentHost)
			if got != tc.want {
				t.Errorf("isRefererCrossOrigin(%q, %q) = %t, want %t", tc.referer, tc.currentHost, got, tc.want)
			}
		})
	}
}

func TestIsSecFetchSiteCrossOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		sfs  string
		want bool
	}{
		{"cross-site", true},
		{"cross-origin", true},
		{"same-origin", false},
		{"same-site", false},
		{"none", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.sfs, func(t *testing.T) {
			got := isSecFetchSiteCrossOrigin(tc.sfs)
			if got != tc.want {
				t.Errorf("isSecFetchSiteCrossOrigin(%q) = %t, want %t", tc.sfs, got, tc.want)
			}
		})
	}
}

// ---------- apiKeyFromRequest ----------

func TestApiKeyFromRequest_QueryApikey(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api?apikey=abc123", nil)
	key, fromCookie := apiKeyFromRequest(r)
	if key != "abc123" || fromCookie {
		t.Errorf("got (%q, %v), want (abc123, false)", key, fromCookie)
	}
}

func TestApiKeyFromRequest_QueryNzbkey(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api?nzbkey=nzb456", nil)
	key, fromCookie := apiKeyFromRequest(r)
	if key != "nzb456" || fromCookie {
		t.Errorf("got (%q, %v), want (nzb456, false)", key, fromCookie)
	}
}

func TestApiKeyFromRequest_Header(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Header.Set("X-API-Key", "header789")
	key, fromCookie := apiKeyFromRequest(r)
	if key != "header789" || fromCookie {
		t.Errorf("got (%q, %v), want (header789, false)", key, fromCookie)
	}
}

func TestApiKeyFromRequest_Cookie(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: "cookie000"})
	key, fromCookie := apiKeyFromRequest(r)
	if key != "cookie000" || !fromCookie {
		t.Errorf("got (%q, %v), want (cookie000, true)", key, fromCookie)
	}
}

func TestApiKeyFromRequest_Priority(t *testing.T) {
	t.Parallel()
	// Query param takes priority over header and cookie.
	r := httptest.NewRequest("GET", "/api?apikey=query", nil)
	r.Header.Set("X-API-Key", "header")
	r.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: "cookie"})
	key, _ := apiKeyFromRequest(r)
	if key != "query" {
		t.Errorf("got %q, want query param to take priority", key)
	}
}

func TestApiKeyFromRequest_None(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	key, _ := apiKeyFromRequest(r)
	if key != "" {
		t.Errorf("got %q, want empty", key)
	}
}

// ---------- callerLevel ----------

func TestCallerLevel_ValidAPIKey(t *testing.T) {
	t.Parallel()
	cfg := AuthConfig{APIKey: "0123456789abcdef", NZBKey: "fedcba9876543210"}
	r := httptest.NewRequest("GET", "/api?apikey=0123456789abcdef", nil)
	r.RemoteAddr = "192.168.1.1:12345"
	if got := callerLevel(r, cfg); got != LevelAdmin {
		t.Errorf("valid API key: got %d, want LevelAdmin", got)
	}
}

func TestCallerLevel_ValidNZBKey(t *testing.T) {
	t.Parallel()
	cfg := AuthConfig{APIKey: "0123456789abcdef", NZBKey: "fedcba9876543210"}
	r := httptest.NewRequest("GET", "/api?apikey=fedcba9876543210", nil)
	r.RemoteAddr = "192.168.1.1:12345"
	if got := callerLevel(r, cfg); got != LevelProtected {
		t.Errorf("valid NZB key: got %d, want LevelProtected", got)
	}
}

func TestCallerLevel_InvalidKey(t *testing.T) {
	t.Parallel()
	cfg := AuthConfig{APIKey: "0123456789abcdef", NZBKey: "fedcba9876543210"}
	r := httptest.NewRequest("GET", "/api?apikey=wrongwrongwrong", nil)
	r.RemoteAddr = "192.168.1.1:12345"
	if got := callerLevel(r, cfg); got != 0 {
		t.Errorf("invalid key: got %d, want 0", got)
	}
}

func TestCallerLevel_MissingKey(t *testing.T) {
	t.Parallel()
	cfg := AuthConfig{APIKey: "0123456789abcdef"}
	r := httptest.NewRequest("GET", "/api", nil)
	r.RemoteAddr = "192.168.1.1:12345"
	if got := callerLevel(r, cfg); got != 0 {
		t.Errorf("no key: got %d, want 0", got)
	}
}

func TestCallerLevel_CookieBlockedByCrossOrigin(t *testing.T) {
	t.Parallel()
	cfg := AuthConfig{APIKey: "0123456789abcdef", SessionKey: "session000session000"}
	r := httptest.NewRequest("GET", "/api", nil)
	r.RemoteAddr = "192.168.1.1:12345"
	r.Host = "localhost:4289"
	r.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: "session000session000"})
	r.Header.Set("Origin", "http://evil.com")
	if got := callerLevel(r, cfg); got != 0 {
		t.Errorf("cookie + cross-origin: got %d, want 0", got)
	}
}

func TestCallerLevel_ValidSessionKeyCookie(t *testing.T) {
	t.Parallel()
	cfg := AuthConfig{APIKey: "0123456789abcdef", SessionKey: "session000session000"}
	r := httptest.NewRequest("GET", "/api", nil)
	r.RemoteAddr = "127.0.0.1:12345" // loopback: trusted for cookie auth (SEC-1)
	r.Host = "localhost:4289"
	r.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: "session000session000"})
	if got := callerLevel(r, cfg); got != LevelAdmin {
		t.Errorf("valid session key cookie: got %d, want LevelAdmin", got)
	}
}

// TestCallerLevel_CookieDoesNotAcceptAPIKey pins the core security property
// of the session key split: the permanent APIKey must not authenticate via
// the cookie path, even though it would authenticate via query/header. A
// leaked browser cookie must never be replayable as the long-lived key used
// by third-party integrations.
func TestCallerLevel_CookieDoesNotAcceptAPIKey(t *testing.T) {
	t.Parallel()
	cfg := AuthConfig{APIKey: "0123456789abcdef", SessionKey: "session000session000"}
	r := httptest.NewRequest("GET", "/api", nil)
	r.RemoteAddr = "127.0.0.1:12345" // loopback: trusted, so this asserts the key check, not the trust gate (SEC-1)
	r.Host = "localhost:4289"
	r.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: "0123456789abcdef"})
	if got := callerLevel(r, cfg); got != 0 {
		t.Errorf("APIKey via cookie: got %d, want 0 (must not authenticate)", got)
	}
}

// ---------- sanitizeQuery ----------

func TestSanitizeQuery_RedactsApikey(t *testing.T) {
	t.Parallel()
	out := sanitizeQuery("mode=queue&apikey=secret123&output=json")
	if out == "" {
		t.Fatal("empty output")
	}
	// The secret value should not appear anywhere in the output.
	if strings.Contains(out, "secret123") {
		t.Errorf("apikey value not redacted: %s", out)
	}
	// The redacted marker should appear (URL-encoded *** = %2A%2A%2A).
	if !strings.Contains(out, "%2A%2A%2A") {
		t.Errorf("redaction marker missing: %s", out)
	}
}

func TestSanitizeQuery_RedactsNzbkey(t *testing.T) {
	t.Parallel()
	out := sanitizeQuery("nzbkey=mysecret&mode=addfile")
	if strings.Contains(out, "mysecret") {
		t.Errorf("nzbkey value not redacted: %s", out)
	}
}

func TestSanitizeQuery_PreservesOtherParams(t *testing.T) {
	t.Parallel()
	out := sanitizeQuery("mode=queue&output=json")
	if out == "" || out == "***" {
		t.Errorf("normal params should be preserved: %s", out)
	}
	if !strings.Contains(out, "mode=queue") {
		t.Errorf("mode param missing: %s", out)
	}
}

func TestSanitizeQuery_Malformed(t *testing.T) {
	t.Parallel()
	out := sanitizeQuery("%;invalid")
	if out != "***" {
		t.Errorf("malformed query: got %q, want ***", out)
	}
}

func TestSanitizeQuery_RedactsSecretParamNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string // substring that must NOT appear in output
	}{
		{"password param", "mode=config&name=test_server&password=hunter2", "hunter2"},
		{"secret param", "mode=addurl&url=http://x&secret=topsecret", "topsecret"},
		{"token param", "mode=addurl&token=abc123", "abc123"},
		{"key param", "mode=addurl&api_key=abc123", "abc123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeQuery(tc.raw)
			if strings.Contains(got, tc.want) {
				t.Errorf("sanitizeQuery(%q) = %q; still contains secret %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestSanitizeQuery_RedactsSecretValueByKeyword(t *testing.T) {
	t.Parallel()
	// set_config&keyword=<field>&value=<secret> — the secret travels in
	// "value", named indirectly via the sibling "keyword" param. Covers all
	// of the config's secret-bearing fields: password (general/servers/
	// notifications), api_key and nzb_key (general) — see
	// internal/config/general.go, servers.go, notifications.go.
	cases := []struct {
		name    string
		keyword string
	}{
		{"password", "password"},
		{"api_key", "api_key"},
		{"nzb_key", "nzb_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeQuery("mode=set_config&keyword=" + tc.keyword + "&value=hunter2")
			if strings.Contains(got, "hunter2") {
				t.Errorf("sanitizeQuery redacted keyword=%s but value leaked: %q", tc.keyword, got)
			}
		})
	}
}

func TestSanitizeQuery_PreservesNonSecretParams(t *testing.T) {
	t.Parallel()
	got := sanitizeQuery("mode=queue&name=delete&value=job123")
	if !strings.Contains(got, "job123") {
		t.Errorf("sanitizeQuery over-redacted a non-secret value: %q", got)
	}
}

// ---------- isMultipartUpload ----------

func TestIsMultipartUpload_True(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/api", nil)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	if !isMultipartUpload(r) {
		t.Error("should detect multipart POST")
	}
}

func TestIsMultipartUpload_GetMethod(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/api", nil)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	if isMultipartUpload(r) {
		t.Error("GET should not be multipart upload")
	}
}

func TestIsMultipartUpload_WrongContentType(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/api", nil)
	r.Header.Set("Content-Type", "application/json")
	if isMultipartUpload(r) {
		t.Error("JSON content-type should not be multipart")
	}
}

// ---------- statusWriter ----------

func TestStatusWriter_DefaultStatus(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	sw.Write([]byte("hello"))
	if sw.status != 200 {
		t.Errorf("default status = %d, want 200", sw.status)
	}
}

func TestStatusWriter_ExplicitStatus(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	sw.WriteHeader(http.StatusNotFound)
	if sw.status != 404 {
		t.Errorf("status = %d, want 404", sw.status)
	}
}

func TestStatusWriter_DoubleWriteHeader(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	sw.WriteHeader(http.StatusCreated)
	sw.WriteHeader(http.StatusInternalServerError) // second call
	if sw.status != 201 {
		t.Errorf("first status should win: got %d, want 201", sw.status)
	}
}

func TestIsCrossOrigin_CookieStateChangingEmptyHeaders(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/api?mode=pause", nil)
	r.Host = "localhost:4289"
	r.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: "sessionkey"})
	if !isCrossOrigin(r) {
		t.Error("cookie-authenticated state-changing request with empty Origin and Referer MUST be cross-origin (fail closed)")
	}
}

// TestCallerLevel_CookieTrustGate exercises the SEC-1 acceptance-side trust
// gate through callerLevel: a valid session cookie authenticates as admin
// only from a trusted source (loopback, or a configured TrustedRange, or a
// verified X-Forwarded-For chain). Same-origin is assured via r.Host so the
// isCrossOrigin gate never short-circuits the cases under test.
func TestCallerLevel_CookieTrustGate(t *testing.T) {
	t.Parallel()
	const sess = "session000session000"
	priv := mustPrefixes(t, "192.168.0.0/16")

	tests := []struct {
		name          string
		remoteAddr    string
		xff           string
		forwarded     string
		xRealIP       string
		ranges        []netip.Prefix
		verifyXFF     bool
		forwardHeader config.TrustedForwardHeader
		want          AccessLevel
	}{
		{name: "loopback trusted by default", remoteAddr: "127.0.0.1:5000", want: LevelAdmin},
		{name: "non-loopback untrusted by default", remoteAddr: "192.168.1.5:5000", want: 0},
		{name: "non-loopback trusted via range", remoteAddr: "192.168.1.5:5000", ranges: priv, want: LevelAdmin},
		{name: "outside range untrusted", remoteAddr: "10.0.0.5:5000", ranges: priv, want: 0},
		{name: "verifyXFF trusted peer + trusted hop", remoteAddr: "192.168.1.5:5000", xff: "192.168.1.9", ranges: priv, verifyXFF: true, want: LevelAdmin},
		{name: "verifyXFF trusted peer + untrusted hop", remoteAddr: "192.168.1.5:5000", xff: "8.8.8.8", ranges: priv, verifyXFF: true, want: 0},
		{name: "spoofed xff ignored when verify off", remoteAddr: "8.8.8.8:5000", xff: "127.0.0.1", want: 0},
		// Issue #94: a same-host reverse proxy makes remoteAddr loopback for
		// every request. Before the fix, an XFF header present with
		// VerifyXFF off was silently ignored and the loopback peer alone
		// granted LevelAdmin — exactly the zero-credential RCE path. Now it
		// must fail closed even though the peer itself is trusted.
		{name: "trusted peer + xff present + verify off fails closed (issue #94)", remoteAddr: "192.168.1.5:5000", xff: "8.8.8.8", ranges: priv, want: 0},
		// Forwarded: (RFC 7239) and X-Real-IP through the full middleware
		// boundary, not just config.IsTrustedRemote's unit tests — proves
		// AuthConfig.ForwardHeader is actually wired through callerLevel.
		{name: "verifyXFF forwarded selector: trusted hop", remoteAddr: "192.168.1.5:5000", forwarded: "for=192.168.1.9", ranges: priv, verifyXFF: true, forwardHeader: config.ForwardHeaderForwarded, want: LevelAdmin},
		{name: "verifyXFF forwarded selector: untrusted hop", remoteAddr: "192.168.1.5:5000", forwarded: "for=8.8.8.8", ranges: priv, verifyXFF: true, forwardHeader: config.ForwardHeaderForwarded, want: 0},
		{name: "verifyXFF x-real-ip selector: trusted", remoteAddr: "192.168.1.5:5000", xRealIP: "192.168.1.9", ranges: priv, verifyXFF: true, forwardHeader: config.ForwardHeaderXRealIP, want: LevelAdmin},
		{name: "verifyXFF x-real-ip selector: untrusted", remoteAddr: "192.168.1.5:5000", xRealIP: "8.8.8.8", ranges: priv, verifyXFF: true, forwardHeader: config.ForwardHeaderXRealIP, want: 0},
		// Header-precedence regression guard at the middleware boundary: a
		// forged xff must not override an x-real-ip selector.
		{name: "x-real-ip selector: forged xff ignored, real x-real-ip rejected", remoteAddr: "192.168.1.5:5000", xff: "192.168.1.9", xRealIP: "8.8.8.8", ranges: priv, verifyXFF: true, forwardHeader: config.ForwardHeaderXRealIP, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := AuthConfig{APIKey: testAPIKey, SessionKey: sess, TrustedRanges: tc.ranges, VerifyXFF: tc.verifyXFF, ForwardHeader: tc.forwardHeader}
			r := httptest.NewRequest("GET", "/api", nil)
			r.Host = "localhost:4289"
			r.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.forwarded != "" {
				r.Header.Set("Forwarded", tc.forwarded)
			}
			if tc.xRealIP != "" {
				r.Header.Set("X-Real-IP", tc.xRealIP)
			}
			r.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: sess})
			if got := callerLevel(r, cfg); got != tc.want {
				t.Errorf("callerLevel = %d; want %d", got, tc.want)
			}
		})
	}
}

func mustPrefixes(t *testing.T, entries ...string) []netip.Prefix {
	t.Helper()
	p, err := config.ParseLocalRanges(entries)
	if err != nil {
		t.Fatalf("ParseLocalRanges(%v): %v", entries, err)
	}
	return p
}
