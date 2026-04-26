package api

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/config"
)

func testServerWithConfig(t *testing.T, cfg *config.Config) *Server {
	if cfg != nil {
		cfg.With(func(c *config.Config) {
			c.General.APIKey = testAPIKey
			c.General.NZBKey = testNZBKey
		})
	}
	t.Helper()
	return New(Options{
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
	s := New(Options{Version: "1.0.0"})
	rr := apiGet(t, s.Handler(), "/api?mode=set_config&apikey="+testAPIKey)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rr.Code)
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

func TestGetConfigConcurrentSafe(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	s := testServerWithConfig(t, cfg)

	// Run set_config and get_config concurrently to ensure no data races
	// when get_config serializes the config structure.

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			rr := apiGet(t, s.Handler(), "/api?mode=set_config&section=general&keyword=download_dir&value=dir"+strconv.Itoa(i)+"&apikey="+testAPIKey)
			if rr.Code != http.StatusOK {
				t.Errorf("set_config failed: %d, body: %s", rr.Code, rr.Body.String())
			}
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		rr := apiGet(t, s.Handler(), "/api?mode=get_config&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Errorf("get_config failed: %d", rr.Code)
		}
	}

	<-done
}
