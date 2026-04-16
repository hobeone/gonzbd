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

func TestModeSetConfig_NotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=set_config&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
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

func TestModeConfig_TestServerNotImplemented(t *testing.T) {
	t.Parallel()
	s := testServer()

	rr := apiGet(t, s.Handler(), "/api?mode=config&name=test_server&apikey="+testAPIKey)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501", rr.Code)
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
