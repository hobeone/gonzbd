package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/api/apitest"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
)

const (
	testAPIKey = "0123456789abcdef"
	testNZBKey = "fedcba9876543210"
)

func testServer() *Server {
	d := dispatch.New(2, 2, time.Hour, time.Now, &apiStubWorkers{}, &apiStubResidency{}, &apiStubStore{}, &apiStubRunner{})
	cfg := &config.Config{General: config.GeneralConfig{APIKey: testAPIKey, NZBKey: testNZBKey}}
	return New(Options{
		Config:     cfg,
		Version:    "1.0.0-test",
		Dispatcher: d,
		App:        apitest.NopApp{Dispatcher: d},
	})
}

func apiGet(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v (body: %s)", err, rr.Body.String())
	}
	return m
}

// --- Response envelope tests ---

func TestRespondOK_WithKeyword(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	respondOK(rr, "version", "1.2.3")
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rr.Code)
	}
	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}
	if m["version"] != "1.2.3" {
		t.Errorf("version = %v; want 1.2.3", m["version"])
	}
}

func TestRespondError(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	s := testServer()
	s.respondError(rr, http.StatusBadRequest, "bad mode")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rr.Code)
	}
	m := decodeJSON(t, rr)
	if m["status"] != false {
		t.Errorf("status = %v; want false", m["status"])
	}
	if m["error"] != "bad mode" {
		t.Errorf("error = %v; want 'bad mode'", m["error"])
	}
}

func TestRespondStatus(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	respondStatus(rr)
	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}
}

// --- Auth middleware tests ---

func TestCallerLevel_NoKey(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=version", nil)
	if got := callerLevel(req, AuthConfig{APIKey: testAPIKey}); got != 0 {
		t.Errorf("level = %d; want 0 (no key)", got)
	}
}

func TestCallerLevel_APIKey(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/api?apikey="+testAPIKey, nil)
	if got := callerLevel(req, AuthConfig{APIKey: testAPIKey}); got != LevelAdmin {
		t.Errorf("level = %d; want %d (admin)", got, LevelAdmin)
	}
}

func TestCallerLevel_NZBKey(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/api?apikey="+testNZBKey, nil)
	cfg := AuthConfig{APIKey: testAPIKey, NZBKey: testNZBKey}
	if got := callerLevel(req, cfg); got != LevelProtected {
		t.Errorf("level = %d; want %d (protected)", got, LevelProtected)
	}
}

func TestCallerLevel_BadKey(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/api?apikey=wrongkey", nil)
	if got := callerLevel(req, AuthConfig{APIKey: testAPIKey}); got != 0 {
		t.Errorf("level = %d; want 0 (bad key)", got)
	}
}

func TestCallerLevel_HeaderKey(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=version", nil)
	req.Header.Set("X-API-Key", testAPIKey)
	if got := callerLevel(req, AuthConfig{APIKey: testAPIKey}); got != LevelAdmin {
		t.Errorf("level = %d; want %d (admin via header)", got, LevelAdmin)
	}
}

// --- Mode dispatch tests ---

func TestModeVersion_NoAuth(t *testing.T) {
	t.Parallel()
	s := testServer()
	rr := apiGet(t, s.Handler(), "/api?mode=version")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	m := decodeJSON(t, rr)
	if m["version"] != "1.0.0-test" {
		t.Errorf("version = %v; want 1.0.0-test", m["version"])
	}
}

func TestModeAuth_ValidKey(t *testing.T) {
	t.Parallel()
	s := testServer()
	rr := apiGet(t, s.Handler(), "/api?mode=auth&apikey="+testAPIKey)
	m := decodeJSON(t, rr)
	if m["auth"] != "apikey" {
		t.Errorf("auth = %v; want apikey", m["auth"])
	}
}

func TestModeAuth_NZBKey(t *testing.T) {
	t.Parallel()
	s := testServer()
	rr := apiGet(t, s.Handler(), "/api?mode=auth&apikey="+testNZBKey)
	m := decodeJSON(t, rr)
	if m["auth"] != "nzbkey" {
		t.Errorf("auth = %v; want nzbkey", m["auth"])
	}
}

func TestModeAuth_BadKey(t *testing.T) {
	t.Parallel()
	s := testServer()
	rr := apiGet(t, s.Handler(), "/api?mode=auth&apikey=wrongkey")
	m := decodeJSON(t, rr)
	if m["auth"] != "badkey" {
		t.Errorf("auth = %v; want badkey", m["auth"])
	}
}

func TestModeAuth_NoKey(t *testing.T) {
	t.Parallel()
	s := testServer()
	rr := apiGet(t, s.Handler(), "/api?mode=auth")
	m := decodeJSON(t, rr)
	if m["auth"] != "apikey" {
		t.Errorf("auth = %v; want apikey (no-key returns apikey per Python)", m["auth"])
	}
}

func TestMissingMode(t *testing.T) {
	t.Parallel()
	s := testServer()
	rr := apiGet(t, s.Handler(), "/api")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rr.Code)
	}
	m := decodeJSON(t, rr)
	if m["error"] != "missing mode parameter" {
		t.Errorf("error = %v", m["error"])
	}
}

func TestUnknownMode(t *testing.T) {
	t.Parallel()
	s := testServer()
	rr := apiGet(t, s.Handler(), "/api?mode=nonexistent&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rr.Code)
	}
}

func TestProtectedMode_NoKey(t *testing.T) {
	t.Parallel()
	// Add a dummy protected mode for this test.
	s := testServer()
	s.modes["test_protected"] = modeEntry{
		handler: func(w http.ResponseWriter, _ *http.Request) {
			respondStatus(w)
		},
		level: LevelProtected,
	}
	rr := apiGet(t, s.Handler(), "/api?mode=test_protected")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", rr.Code)
	}
}

func TestProtectedMode_WithKey(t *testing.T) {
	t.Parallel()
	s := testServer()
	s.modes["test_protected"] = modeEntry{
		handler: func(w http.ResponseWriter, _ *http.Request) {
			respondStatus(w)
		},
		level: LevelProtected,
	}
	rr := apiGet(t, s.Handler(), "/api?mode=test_protected&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rr.Code)
	}
}

func TestAdminMode_NZBKey_Insufficient(t *testing.T) {
	t.Parallel()
	s := testServer()
	s.modes["test_admin"] = modeEntry{
		handler: func(w http.ResponseWriter, _ *http.Request) {
			respondStatus(w)
		},
		level: LevelAdmin,
	}
	rr := apiGet(t, s.Handler(), "/api?mode=test_admin&apikey="+testNZBKey)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403 (NZB key insufficient for admin)", rr.Code)
	}
}

func TestAdminMode_APIKey_Sufficient(t *testing.T) {
	t.Parallel()
	s := testServer()
	s.modes["test_admin"] = modeEntry{
		handler: func(w http.ResponseWriter, _ *http.Request) {
			respondStatus(w)
		},
		level: LevelAdmin,
	}
	rr := apiGet(t, s.Handler(), "/api?mode=test_admin&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rr.Code)
	}
}

func TestSessionKey_GrantsAdminViaCookie(t *testing.T) {
	t.Parallel()
	s := testServer()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=queue", nil)
	req.RemoteAddr = "127.0.0.1:12345" // loopback: trusted for cookie auth (SEC-1)
	req.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: s.SessionKey()})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestSessionKey_DoesNotAcceptAPIKey(t *testing.T) {
	t.Parallel()
	s := testServer()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=queue", nil)
	req.AddCookie(&http.Cookie{Name: "gonzbd_apikey", Value: testAPIKey})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 (the permanent APIKey must not authenticate via cookie)", rr.Code)
	}
}

func TestSessionKey_UniquePerServerInstance(t *testing.T) {
	t.Parallel()
	s1 := testServer()
	s2 := testServer()
	if s1.SessionKey() == "" {
		t.Fatal("SessionKey() must not be empty")
	}
	if s1.SessionKey() == s2.SessionKey() {
		t.Error("two server instances must not share a session key")
	}
}

func TestAuthConfigDynamic(t *testing.T) {
	t.Parallel()
	d := dispatch.New(2, 2, time.Hour, time.Now, &apiStubWorkers{}, &apiStubResidency{}, &apiStubStore{}, &apiStubRunner{})
	cfg := &config.Config{
		General: config.GeneralConfig{
			APIKey: "old-key",
		},
	}
	s := New(Options{
		Config:     cfg,
		Version:    "1.0.0-test",
		Dispatcher: d,
		App:        apitest.NopApp{Dispatcher: d},
	})

	// Authenticate with old-key works
	rr := apiGet(t, s.Handler(), "/api?mode=version&apikey=old-key")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with old key, got %d", rr.Code)
	}

	// Update key via config
	cfg.With(func(c *config.Config) {
		c.General.APIKey = "new-key"
	})

	// Old key should now fail (for protected routes)
	// Actually, mode=version is LevelOpen, so we need a protected route
	rr = apiGet(t, s.Handler(), "/api?mode=queue&apikey=old-key")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with old key after update, got %d", rr.Code)
	}

	// New key should work
	rr = apiGet(t, s.Handler(), "/api?mode=queue&apikey=new-key")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with new key, got %d", rr.Code)
	}
}

// --- Third-party app compatibility: mode/apikey in POST form body ---

func TestModeFromURLEncodedFormBody(t *testing.T) {
	t.Parallel()
	s := testServer()
	// POST with mode=version in URL-encoded form body, no query param.
	body := strings.NewReader("mode=version")
	req := httptest.NewRequest(http.MethodPost, "/api", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	if m["version"] != "1.0.0-test" {
		t.Errorf("version = %v; want 1.0.0-test", m["version"])
	}
}

func TestModeFromMultipartFormBody(t *testing.T) {
	t.Parallel()
	s := testServer()
	// POST with mode=version as a multipart form field.
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("mode", "version"); err != nil {
		t.Fatal(err)
	}
	w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	if m["version"] != "1.0.0-test" {
		t.Errorf("version = %v; want 1.0.0-test", m["version"])
	}
}

func TestAPIKeyFromURLEncodedFormBody(t *testing.T) {
	t.Parallel()
	s := testServer()
	// POST with both mode and apikey in URL-encoded form body.
	body := strings.NewReader("mode=queue&apikey=" + testAPIKey)
	req := httptest.NewRequest(http.MethodPost, "/api", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestAPIKeyFromMultipartFormBody(t *testing.T) {
	t.Parallel()
	s := testServer()
	// POST with mode and apikey as multipart form fields.
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("mode", "queue"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("apikey", testAPIKey); err != nil {
		t.Fatal(err)
	}
	w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestModeQueryParamTakesPrecedence(t *testing.T) {
	t.Parallel()
	s := testServer()
	// If mode is in BOTH query string and form body, query wins.
	body := strings.NewReader("mode=shutdown")
	req := httptest.NewRequest(http.MethodPost, "/api?mode=version", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	m := decodeJSON(t, rr)
	// Should be version response, not shutdown (which requires admin auth).
	if _, ok := m["version"]; !ok {
		t.Errorf("expected version response, got: %v", m)
	}
}

func TestMissingMode_GETStillFails(t *testing.T) {
	t.Parallel()
	s := testServer()
	// GET without mode= should still fail (form body fallback is POST-only).
	rr := apiGet(t, s.Handler(), "/api")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rr.Code)
	}
}
