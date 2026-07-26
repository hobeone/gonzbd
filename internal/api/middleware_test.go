package api

import (
	"bytes"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

// ---------- isCrossOrigin ----------

// TestIsCrossOrigin covers the general isCrossOrigin cases: no headers, Origin
// header variants (same-host, loopback, external, malformed), Sec-Fetch-Site
// variants, Referer-only cases, and the fail-closed cookie+state-changing
// request with no origin headers. Port-sensitivity edge cases for loopback
// aliases are covered separately in TestIsCrossOrigin_LoopbackPortCases.
func TestIsCrossOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		method   string
		host     string
		origin   string
		secFetch string // Sec-Fetch-Site value
		referer  string
		cookie   string // gonzbd_apikey cookie value; empty = no cookie
		want     bool   // want cross-origin
	}{
		// No headers at all: no signal → not cross-origin.
		{"no headers", "GET", "localhost:4289", "", "", "", "", false},

		// Origin header cases.
		{"same-host Origin", "GET", "localhost:4289", "http://localhost:4289", "", "", "", false},
		{"loopback IP Origin", "GET", "localhost:4289", "http://127.0.0.1:4289", "", "", "", false},
		{"external Origin", "GET", "localhost:4289", "http://evil.example.com", "", "", "", true},
		{"malformed Origin", "GET", "localhost:4289", "://bad", "", "", "", true},
		// PR #139: matching Origin must not short-circuit when Sec-Fetch-Site
		// disagrees — both signals must agree the request is same-origin.
		{"same-host Origin + Sec-Fetch-Site cross-site", "GET", "localhost:4289", "http://localhost:4289", "cross-site", "", "", true},

		// Sec-Fetch-Site header cases (no Origin).
		{"Sec-Fetch-Site cross-site", "GET", "localhost:4289", "", "cross-site", "", "", true},
		{"Sec-Fetch-Site cross-origin", "GET", "localhost:4289", "", "cross-origin", "", "", true},
		{"Sec-Fetch-Site same-origin", "GET", "localhost:4289", "", "same-origin", "", "", false},

		// Referer-only cases (no Origin, no Sec-Fetch-Site).
		{"Referer cross-origin", "GET", "localhost:4289", "", "", "http://evil.com/page", "", true},
		{"Referer same host", "GET", "localhost:4289", "", "", "http://localhost:4289/config", "", false},
		{"Referer loopback IP", "GET", "localhost:4289", "", "", "http://127.0.0.1:4289/page", "", false},

		// Fail-closed: cookie-authenticated state-changing POST with no Origin
		// or Referer MUST be treated as cross-origin (prevents CSRF via omitted
		// headers).
		{"cookie POST no origin/referer", "POST", "localhost:4289", "", "", "", "sessionkey", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(tc.method, "/api?mode=pause", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.secFetch != "" {
				r.Header.Set("Sec-Fetch-Site", tc.secFetch)
			}
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: tc.cookie})
			}
			if got := isCrossOrigin(r, AuthConfig{}); got != tc.want {
				t.Errorf("isCrossOrigin() = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestIsCrossOrigin_LoopbackPortCases covers issue #103 (loopback-alias
// same-origin checks must be port-sensitive) and the code-review follow-up
// fixing a bracketed-IPv6-with-no-port regression.
func TestIsCrossOrigin_LoopbackPortCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string
		host   string
		origin string
		want   bool // want cross-origin
	}{
		{"localhost name, different port MUST be cross-origin", "GET", "127.0.0.1:4289", "http://localhost:9090", true},
		{
			"attack vector: localhost origin on a different port MUST be cross-origin",
			"POST", "localhost:8080", "http://localhost:1234", true,
		},
		{"same-port IPv6 loopback alias should not be cross-origin", "GET", "localhost:4289", "http://[::1]:4289", false},
		{"different-port IPv6 loopback alias MUST be cross-origin", "GET", "localhost:4289", "http://[::1]:9090", true},
		{"bracketed IPv6 loopback, no port on either side, should not be cross-origin", "GET", "localhost", "http://[::1]", false},
		{"bare hostname, no port on either side, should not be cross-origin", "GET", "localhost", "http://localhost", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/api", nil)
			r.Host = tc.host
			r.Header.Set("Origin", tc.origin)
			if got := isCrossOrigin(r, AuthConfig{}); got != tc.want {
				t.Errorf("isCrossOrigin(Host=%q, Origin=%q) = %t, want %t", tc.host, tc.origin, got, tc.want)
			}
		})
	}
}

// TestIsCrossOrigin_DefaultPortCases covers issue #134 (implicit default
// ports 80/443 must be normalized when comparing loopback aliases).
func TestIsCrossOrigin_DefaultPortCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		host       string
		origin     string
		isTLS      bool
		fwdProto   string
		remoteAddr string
		want       bool // want cross-origin
	}{
		{"explicit port 80 Origin vs implicit port 80 Host", "localhost", "http://localhost:80", false, "", "", false},
		{"implicit port 80 Origin vs explicit port 80 Host", "localhost:80", "http://localhost", false, "", "", false},
		{"implicit port 80 loopback IP vs implicit port 80 Host", "localhost", "http://127.0.0.1", false, "", "", false},
		{"explicit port 443 Origin vs implicit port 443 HTTPS Host", "localhost", "https://localhost:443", true, "", "", false},
		{"implicit port 443 Origin vs explicit port 443 HTTPS Host", "localhost:443", "https://localhost", true, "", "", false},
		{"proxy-terminated TLS: trusted loopback peer with X-Forwarded-Proto https", "localhost", "https://localhost", false, "https", "127.0.0.1:12345", false},
		{"untrusted peer: X-Forwarded-Proto https ignored from non-loopback peer", "localhost", "https://localhost", false, "https", "192.0.2.1:12345", true},
		{"different port: 8080 Origin vs implicit port 80 Host", "localhost", "http://localhost:8080", false, "", "", true},
		{"scheme mismatch: http Origin on 443 vs https Host on 443", "localhost", "http://localhost:443", true, "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest("POST", "/api", nil)
			r.Host = tc.host
			r.Header.Set("Origin", tc.origin)
			if tc.isTLS {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.fwdProto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.fwdProto)
			}
			if tc.remoteAddr != "" {
				r.RemoteAddr = tc.remoteAddr
			}
			if got := isCrossOrigin(r, AuthConfig{}); got != tc.want {
				t.Errorf("isCrossOrigin(Host=%q, Origin=%q, TLS=%t, FwdProto=%q, RemoteAddr=%q) = %t, want %t", tc.host, tc.origin, tc.isTLS, tc.fwdProto, tc.remoteAddr, got, tc.want)
			}
		})
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
		{"localhost name referer", "http://localhost:9090/page", "127.0.0.1:4289", true},
		{"attack vector: different port, same loopback name", "http://localhost:1234/page", "localhost:8080", true},
		{"same-port IPv6 loopback referer", "http://[::1]:4289/page", "localhost:4289", false},
		{"different-port IPv6 loopback referer", "http://[::1]:9090/page", "localhost:4289", true},
		{"bare hostname no port both sides", "http://localhost/page", "localhost", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.Host = tc.currentHost
			got := isRefererCrossOrigin(tc.referer, r, AuthConfig{})
			if got != tc.want {
				t.Errorf("isRefererCrossOrigin(%q, %q) = %t, want %t", tc.referer, tc.currentHost, got, tc.want)
			}
		})
	}
}

func TestSameOrigin_DefaultPortNormalization(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		hostportA, schemeA string
		hostportB, schemeB string
		want               bool
	}{
		{"http default port 80 explicit vs implicit", "localhost:80", "http", "localhost", "http", true},
		{"https default port 443 explicit vs implicit", "localhost:443", "https", "localhost", "https", true},
		{"loopback alias with explicit port 80 vs implicit", "127.0.0.1:80", "http", "localhost", "http", true},
		{"non-default port 8080 vs explicit 80", "localhost:8080", "http", "localhost:80", "http", false},
		{"http port 80 vs https port 443", "localhost", "http", "localhost", "https", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sameOrigin(tc.hostportA, tc.schemeA, tc.hostportB, tc.schemeB)
			if got != tc.want {
				t.Errorf("sameOrigin(%q, %q, %q, %q) = %t, want %t", tc.hostportA, tc.schemeA, tc.hostportB, tc.schemeB, got, tc.want)
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
		{name: "trusted ranged peer + xff present + verify off fails closed", remoteAddr: "192.168.1.5:5000", xff: "8.8.8.8", ranges: priv, want: 0},
		// The literal real-world scenario from issue #94: a same-host
		// reverse proxy makes RemoteAddr loopback, which is trusted via
		// ipTrusted's IsLoopback() shortcut — a different code path than
		// the ranged-peer case above (that one goes through the
		// local_ranges containment check instead).
		{name: "loopback peer + xff present + verify off fails closed (issue #94)", remoteAddr: "127.0.0.1:5000", xff: "8.8.8.8", want: 0},
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

// ---------- loggingMiddleware: consolidated single-line logging ----------

// TestLoggingMiddleware_ConsolidatesErrorIntoSingleLine proves that a
// failing request (401 from a missing API key) produces exactly one log
// line, at Warn level, carrying both the source IP and the human-readable
// error message — not a separate "api response warning" line plus a
// duplicate Info-level "api" line for the same request.
func TestLoggingMiddleware_ConsolidatesErrorIntoSingleLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	s := New(Options{Logger: logger})

	req := httptest.NewRequest(http.MethodGet, "/api?mode=queue", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	rr := httptest.NewRecorder()

	s.Handler().ServeHTTP(rr, req)

	out := strings.TrimSpace(buf.String())
	lines := strings.Split(out, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d:\n%s", len(lines), out)
	}
	line := lines[0]

	if !strings.Contains(line, "level=WARN") {
		t.Errorf("expected level=WARN, got: %s", line)
	}
	if !strings.Contains(line, "203.0.113.7") {
		t.Errorf("expected source IP 203.0.113.7 in log line, got: %s", line)
	}
	if !strings.Contains(line, `error="API key required"`) {
		t.Errorf("expected error message in log line, got: %s", line)
	}
	if !strings.Contains(line, "status=401") {
		t.Errorf("expected status=401 in log line, got: %s", line)
	}
}
