package api

import (
	"net/http"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/config"
)

func testServerWithConfig(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	return New(Options{
		Auth: AuthConfig{
			APIKey: testAPIKey,
			NZBKey: testNZBKey,
		},
		Version: "1.0.0-test",
		Config:  cfg,
	})
}

func TestModeGetConfig_Default(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	s := testServerWithConfig(t, cfg)

	rr := apiGet(t, s.Handler(), "/api?mode=get_config&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != true {
		t.Errorf("status = %v; want true", m["status"])
	}

	if _, ok := m["config"]; !ok {
		t.Errorf("config key missing from response")
	}
}

func TestModeSetConfig_NoConfigWired(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=set_config&apikey="+testAPIKey)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 (config not wired)", rr.Code)
	}

	m := decodeJSON(t, rr)
	if m["status"] != false {
		t.Errorf("status = %v; want false", m["status"])
	}
}

func TestModeConfig_SpeedlimitNotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=config&name=speedlimit&value=500&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}
}

func TestModeConfig_TestServer_MissingHost(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=config&name=test_server&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rr.Code)
	}
	m := decodeJSON(t, rr)
	if m["error"] != "missing host parameter" {
		t.Errorf("error = %v; want 'missing host parameter'", m["error"])
	}
}

func TestModeConfig_TestServer_UnreachableHost(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=config&name=test_server&host=192.0.2.1&port=119&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	m := decodeJSON(t, rr)
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing or not a map: %v", m["result"])
	}
	if result["passed"] != false {
		t.Errorf("passed = %v; want false", result["passed"])
	}
	msg, _ := result["message"].(string)
	if msg == "" {
		t.Error("message should be non-empty for a failed connection")
	}
}

func TestModeConfig_CreateBackupNotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=config&name=create_backup&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
	}
}

func TestModeConfig_UnknownAction(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=config&name=unknown&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rr.Code)
	}
}
