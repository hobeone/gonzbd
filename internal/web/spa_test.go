package web

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// alwaysTrusted is a trust predicate that treats every request as trusted,
// used by tests that assert cookie-issuance behavior independent of the
// source-IP gate (which is exercised separately in TestSPACookie_TrustGate).
func alwaysTrusted(*http.Request) bool { return true }

func testSPAFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":   {Data: []byte(`<!DOCTYPE html><html><body>GoNZBD</body></html>`)},
		"_app/test.js": {Data: []byte(`console.log("test")`)},
		"robots.txt":   {Data: []byte("User-agent: *\nDisallow:")},
		"favicon.ico":  {Data: []byte("fake-icon-bytes")},
	}
}

func TestNewSPAHandler_RootServesIndexHTML(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, alwaysTrusted)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "GoNZBD") {
		t.Errorf("GET / body missing 'GoNZBD'")
	}
}

func TestNewSPAHandler_StaticFileServedDirectly(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, alwaysTrusted)

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{"/_app/test.js", http.StatusOK, `console.log`},
		{"/robots.txt", http.StatusOK, "User-agent"},
		{"/favicon.ico", http.StatusOK, "fake-icon-bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest("GET", tt.path, nil))
			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rr.Code, tt.wantStatus)
			}
			if !strings.Contains(rr.Body.String(), tt.wantBody) {
				t.Errorf("body missing %q", tt.wantBody)
			}
		})
	}
}

func TestNewSPAHandler_UnknownPathFallsBackToIndex(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, alwaysTrusted)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/some/deep/route", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /some/deep/route status = %d, want 200 (SPA catch-all)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "GoNZBD") {
		t.Errorf("SPA catch-all body missing 'GoNZBD' (should serve index.html)")
	}
}

func TestNewSPAHandler_MissingFileWithExtensionReturns404(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, alwaysTrusted)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/_app/does-not-exist.js", nil))

	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /_app/does-not-exist.js status = %d, want 404", rr.Code)
	}
}

func TestSPACookieOnRoot(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, alwaysTrusted)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))

	cookies := rr.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == "gonzbd_apikey" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatalf("GET / did not set gonzbd_apikey cookie")
	}
	if found.Value != "test-key" {
		t.Errorf("cookie value = %q, want 'test-key'", found.Value)
	}
}

func TestSPACookieOnDeepLink(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, alwaysTrusted)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/config/general", nil))

	cookies := rr.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == "gonzbd_apikey" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatalf("GET /config/general did not set gonzbd_apikey cookie")
	}
	if found.Value != "test-key" {
		t.Errorf("cookie value = %q, want 'test-key'", found.Value)
	}
}

func TestStaticAssetNoCookie(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, alwaysTrusted)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/_app/test.js", nil))

	cookies := rr.Result().Cookies()
	for _, c := range cookies {
		if c.Name == "gonzbd_apikey" {
			t.Errorf("GET /_app/test.js should not set gonzbd_apikey cookie")
		}
	}
}

func TestSPACookieSecureFlag(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, alwaysTrusted)

	t.Run("with TLS (HTTPS)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.TLS = &tls.ConnectionState{}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		cookies := rr.Result().Cookies()
		var found *http.Cookie
		for _, c := range cookies {
			if c.Name == "gonzbd_apikey" {
				found = c
				break
			}
		}
		if found == nil {
			t.Fatalf("did not set gonzbd_apikey cookie")
		}
		if !found.Secure {
			t.Error("cookie should have Secure=true when request is HTTPS (TLS != nil)")
		}
	})

	t.Run("without TLS (HTTP)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.TLS = nil
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		cookies := rr.Result().Cookies()
		var found *http.Cookie
		for _, c := range cookies {
			if c.Name == "gonzbd_apikey" {
				found = c
				break
			}
		}
		if found == nil {
			t.Fatalf("did not set gonzbd_apikey cookie")
		}
		if found.Secure {
			t.Error("cookie should have Secure=false when request is HTTP (TLS == nil)")
		}
	})
}

func TestSPACookieSecureFlag_XForwardedProto(t *testing.T) {
	tests := []struct {
		proto string
		want  bool
	}{
		{"https", true},
		{"HTTPS", true},
		{"Https", true},
		{"http", false},
		{"HTTP", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.proto, func(t *testing.T) {
			handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, alwaysTrusted)
			req := httptest.NewRequest("GET", "/", nil)
			if tt.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tt.proto)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			cookies := rr.Result().Cookies()
			var found *http.Cookie
			for _, c := range cookies {
				if c.Name == "gonzbd_apikey" {
					found = c
					break
				}
			}
			if found == nil {
				t.Fatalf("did not set gonzbd_apikey cookie")
			}
			if found.Secure != tt.want {
				t.Errorf("Secure = %v, want %v for X-Forwarded-Proto = %q", found.Secure, tt.want, tt.proto)
			}
		})
	}
}

// TestSPACookie_TrustGate is the SEC-1 issuance-side regression: an
// untrusted client (trustedFn returns false) must NOT receive the admin
// session cookie from GET /, while a trusted client must. Without the gate,
// GET / handed the cookie to anyone.
func TestSPACookie_TrustGate(t *testing.T) {
	hasCookie := func(rr *httptest.ResponseRecorder) bool {
		for _, c := range rr.Result().Cookies() {
			if c.Name == "gonzbd_apikey" {
				return true
			}
		}
		return false
	}

	t.Run("untrusted client gets SPA but no cookie", func(t *testing.T) {
		untrusted := func(*http.Request) bool { return false }
		handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, untrusted)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET / status = %d, want 200 (SPA still served)", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "GoNZBD") {
			t.Error("untrusted client should still receive the SPA HTML")
		}
		if hasCookie(rr) {
			t.Error("untrusted client must NOT receive the gonzbd_apikey cookie")
		}
	})

	t.Run("untrusted client gets no cookie on deep-link catch-all", func(t *testing.T) {
		untrusted := func(*http.Request) bool { return false }
		handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, untrusted)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", "/config/general", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /config/general status = %d, want 200 (SPA still served via catch-all)", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "GoNZBD") {
			t.Error("untrusted client should still receive the SPA HTML via the deep-link catch-all")
		}
		if hasCookie(rr) {
			t.Error("untrusted client must NOT receive the cookie via SPA catch-all")
		}
	})

	t.Run("trusted client gets cookie", func(t *testing.T) {
		handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, alwaysTrusted)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
		if !hasCookie(rr) {
			t.Error("trusted client must receive the cookie")
		}
	})
}

func TestNewSPAHandler_SetCookie(t *testing.T) {
	handler := NewSPAHandler(testSPAFS(), func() string { return "test-key" }, alwaysTrusted)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))

	cookies := rr.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == "gonzbd_apikey" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatalf("GET / did not set gonzbd_apikey cookie")
	}
	if found.Value != "test-key" {
		t.Errorf("cookie value = %q, want 'test-key'", found.Value)
	}
	if !found.HttpOnly {
		t.Error("cookie should have HttpOnly=true")
	}
}
