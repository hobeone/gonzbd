package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleAPI_CookieAuth_GET_StateChangingRestricted_405(t *testing.T) {
	t.Parallel()
	s := testServer()

	stateChangingModes := []struct {
		name string
		path string
	}{
		{"set_config", "/api?mode=set_config&section=general&keyword=port&value=8080"},
		{"shutdown", "/api?mode=shutdown"},
		{"restart", "/api?mode=restart"},
		{"pause", "/api?mode=pause"},
		{"resume", "/api?mode=resume"},
		{"disconnect", "/api?mode=disconnect"},
		{"queue_delete", "/api?mode=queue&name=delete&value=SABnzbd_nzo_123"},
		{"queue_purge", "/api?mode=queue&name=purge"},
		{"queue_delete_nzf", "/api?mode=queue&name=delete_nzf&value=abc"},
		{"history_delete", "/api?mode=history&name=delete&value=SABnzbd_nzo_456"},
	}

	for _, tc := range stateChangingModes {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Host = "localhost:4289"
			req.RemoteAddr = "127.0.0.1:12345" // loopback: trusted for cookie auth (SEC-1)
			req.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: s.SessionKey()})
			rr := httptest.NewRecorder()

			s.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s via GET with cookie auth: got status %d, want %d (405 Method Not Allowed)", tc.name, rr.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

func TestHandleAPI_ExplicitAuth_GET_StateChangingAllowed(t *testing.T) {
	t.Parallel()
	s := testServer()

	explicitAuthCases := []struct {
		name string
		path string
	}{
		{"shutdown", "/api?mode=shutdown&apikey=" + testAPIKey},
		{"pause", "/api?mode=pause&apikey=" + testAPIKey},
		{"queue_delete", "/api?mode=queue&name=delete&value=all&apikey=" + testAPIKey},
		{"history_delete", "/api?mode=history&name=delete&value=all&apikey=" + testAPIKey},
	}

	for _, tc := range explicitAuthCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rr := httptest.NewRecorder()

			s.Handler().ServeHTTP(rr, req)

			if rr.Code == http.StatusMethodNotAllowed {
				t.Errorf("%s via GET with explicit apikey: got 405 Method Not Allowed, but explicit API key must allow GET for 3rd-party compatibility", tc.name)
			}
		})
	}
}

func TestHandleAPI_CookieAuth_POST_StateChangingAllowed(t *testing.T) {
	t.Parallel()
	s := testServer()

	req := httptest.NewRequest(http.MethodPost, "/api?mode=pause", nil)
	req.Host = "localhost:4289"
	req.RemoteAddr = "127.0.0.1:12345" // loopback: trusted for cookie auth (SEC-1)
	req.Header.Set("Origin", "http://localhost:4289")
	req.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: s.SessionKey()})
	rr := httptest.NewRecorder()

	s.Handler().ServeHTTP(rr, req)

	if rr.Code == http.StatusMethodNotAllowed {
		t.Errorf("pause via POST with cookie auth: got 405 Method Not Allowed, should be allowed")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("pause via POST with cookie auth: got status %d, want 200 OK", rr.Code)
	}
}

func TestHandleAPI_CookieAuth_NonPost_StateChangingRestricted_405(t *testing.T) {
	t.Parallel()
	s := testServer()

	methods := []string{http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api?mode=pause", nil)
			req.Host = "localhost:4289"
			req.RemoteAddr = "127.0.0.1:12345" // loopback: trusted for cookie auth (SEC-1)
			req.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: s.SessionKey()})
			rr := httptest.NewRecorder()

			s.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("pause via %s with cookie auth: got status %d, want 405 Method Not Allowed", method, rr.Code)
			}
		})
	}
}

// TestSEC1_UntrustedCookieRejected is the SEC-1 acceptance-side regression:
// replaying a VALID session cookie from a non-loopback address (the
// "GET / to grab the cookie, then replay it" attack) must NOT grant admin.
// The loopback-only default trust policy rejects the cookie; the same cookie
// from loopback is accepted. Read-only mode=queue is used so the only
// variable is the auth outcome (200 = authenticated, 401 = rejected).
func TestSEC1_UntrustedCookieRejected(t *testing.T) {
	t.Parallel()
	s := testServer()

	newReq := func(remoteAddr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api?mode=queue", nil)
		req.Host = "localhost:4289"
		req.RemoteAddr = remoteAddr
		req.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: s.SessionKey()})
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	// Non-loopback source replaying the valid cookie: rejected. The trust
	// gate makes callerLevel return 0, so the router responds 401 ("API key
	// required") — the cookie authenticated nothing.
	if rr := newReq("203.0.113.7:5000"); rr.Code != http.StatusUnauthorized {
		t.Errorf("valid cookie from non-loopback: got status %d, want 401 (cookie must not authenticate an untrusted source)", rr.Code)
	}
	// Docker-bridge-style private source, still untrusted by default: rejected.
	if rr := newReq("172.18.0.9:5000"); rr.Code != http.StatusUnauthorized {
		t.Errorf("valid cookie from untrusted private addr: got status %d, want 401", rr.Code)
	}
	// Loopback source: accepted.
	if rr := newReq("127.0.0.1:5000"); rr.Code != http.StatusOK {
		t.Errorf("valid cookie from loopback: got status %d, want 200 (trusted)", rr.Code)
	}
}

// TestHandleAPI_CookieAuth_GET_StateChangingClassification guards the
// fail-closed CSRF classification. Before it, several genuinely
// state-changing sub-actions (pause_all, rename, change_cat, change_script,
// history retry/mark_as_completed, warnings clear, and every mode=config
// sub-action) were not classified as mutations, so a cookie-authenticated
// GET (the CSRF vector) sailed past the POST-only gate.
func TestHandleAPI_CookieAuth_GET_StateChangingClassification(t *testing.T) {
	t.Parallel()
	s := testServer()

	blocked := []struct{ name, url string }{
		{"queue pause_all", "/api?mode=queue&name=pause_all"},
		{"queue resume_all", "/api?mode=queue&name=resume_all"},
		{"queue rename", "/api?mode=queue&name=rename&value=x&value2=y"},
		{"queue change_cat", "/api?mode=queue&name=change_cat&value=x&value2=y"},
		{"queue change_script", "/api?mode=queue&name=change_script&value=x&value2=y"},
		{"history retry", "/api?mode=history&name=retry&value=x"},
		{"history mark_as_completed", "/api?mode=history&name=mark_as_completed&value=x"},
		{"warnings clear", "/api?mode=warnings&name=clear"},
		{"config set_apikey", "/api?mode=config&name=set_apikey"},
		{"config set_pause", "/api?mode=config&name=set_pause&value=1"},
	}
	for _, tc := range blocked {
		t.Run("blocked/"+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			req.Host = "localhost:4289"
			req.RemoteAddr = "127.0.0.1:12345" // loopback: trusted for cookie auth (SEC-1)
			req.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: s.SessionKey()})
			rr := httptest.NewRecorder()

			s.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s via cookie GET: got status %d, want 405 (state-changing must require POST)", tc.name, rr.Code)
			}
		})
	}

	// Read-only cookie GETs must NOT be blocked. They may return other
	// statuses depending on wiring, but never 405.
	allowed := []struct{ name, url string }{
		{"queue list", "/api?mode=queue"},
		{"history list", "/api?mode=history"},
		{"warnings poll", "/api?mode=warnings"},
	}
	for _, tc := range allowed {
		t.Run("allowed/"+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			req.Host = "localhost:4289"
			req.RemoteAddr = "127.0.0.1:12345" // loopback: trusted for cookie auth (SEC-1)
			req.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: s.SessionKey()})
			rr := httptest.NewRecorder()

			s.Handler().ServeHTTP(rr, req)

			if rr.Code == http.StatusMethodNotAllowed {
				t.Errorf("%s via cookie GET: got 405, but a read-only action must be allowed", tc.name)
			}
		})
	}
}
