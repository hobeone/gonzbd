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
