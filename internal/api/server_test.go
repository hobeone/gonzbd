package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

const (
	testAPIKey = "0123456789abcdef"
	testNZBKey = "fedcba9876543210"
)

type mockApp struct {
	q *queue.Queue
	h *history.Repository
}

func (m mockApp) ReloadDownloader([]config.ServerConfig) error { return nil }
func (m mockApp) RetryHistoryJob(context.Context, string) error { return nil }
func (m mockApp) AddJob(ctx context.Context, job *queue.Job, rawNZB []byte) error {
	if m.q == nil {
		return fmt.Errorf("queue not wired to mockApp")
	}
	return m.q.Add(job)
}
func (m mockApp) RemoveJob(id string, deleteFiles bool) error {
	if m.q == nil {
		return fmt.Errorf("queue not wired to mockApp")
	}
	return m.q.Remove(id)
}
func (m mockApp) RemoveHistoryJob(ctx context.Context, id string, deleteFiles bool) error {
	if m.h == nil {
		return fmt.Errorf("history not wired to mockApp")
	}
	_, err := m.h.Delete(ctx, id)
	return err
}

func testServer() *Server {
	q := queue.New()
	return New(Options{
		Auth: AuthConfig{
			APIKey:          testAPIKey,
			NZBKey:          testNZBKey,
			LocalhostBypass: false,
		},
		Version: "1.0.0-test",
		Queue:   q,
		App:     mockApp{q: q},
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
	respondError(rr, http.StatusBadRequest, "bad mode")
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

func TestCallerLevel_LocalhostBypass(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=version", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	cfg := AuthConfig{APIKey: testAPIKey, LocalhostBypass: true}
	if got := callerLevel(req, cfg); got != LevelAdmin {
		t.Errorf("level = %d; want %d (admin via localhost)", got, LevelAdmin)
	}
}

func TestCallerLevel_LocalhostBypassDisabled(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=version", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	cfg := AuthConfig{APIKey: testAPIKey, LocalhostBypass: false}
	if got := callerLevel(req, cfg); got != 0 {
		t.Errorf("level = %d; want 0 (localhost bypass disabled, no key)", got)
	}
}

func TestCallerLevel_IPv6Localhost(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/api?mode=version", nil)
	req.RemoteAddr = "[::1]:54321"
	cfg := AuthConfig{APIKey: testAPIKey, LocalhostBypass: true}
	if got := callerLevel(req, cfg); got != LevelAdmin {
		t.Errorf("level = %d; want %d (admin via ::1)", got, LevelAdmin)
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

// --- Integration: Start + real HTTP ---

func TestServer_StartShutdown(t *testing.T) {
	t.Parallel()
	s := testServer()
	addr, err := s.Start(":0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("listening on %s", addr)

	resp, err := http.Get("http://" + addr.String() + "/api?mode=version") //nolint:gosec // G107: test URL from ephemeral port
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // test cleanup
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}

	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
