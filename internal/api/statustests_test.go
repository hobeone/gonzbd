package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/hobeone/gonzbd/internal/api/apitest"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
)

func TestModeStatus_TestConnection_UnknownServer(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Config: cfg, App: &app.Application{}})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_connection&value=nonexistent&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %v", m)
	}
	if result["ok"] != false {
		t.Errorf("result.ok = %v; want false for unknown server", result["ok"])
	}
}

func TestModeStatus_TestConnection_MissingValue(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Config: cfg, App: &app.Application{}})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_connection&apikey="+testAPIKey)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 when value (server name) is missing", rr.Code)
	}
}

func TestModeStatus_TestConnection_UnreachableHost(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) {
		c.General.APIKey = testAPIKey
		c.Servers = []config.ServerConfig{{
			Name: "s1", Host: "127.0.0.1", Port: 1, // port 1 should refuse/timeout quickly
			Connections: 1, Timeout: 2,
		}}
	})
	s := New(Options{Config: cfg, App: &app.Application{}})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_connection&value=s1&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result := m["result"].(map[string]any)
	if result["ok"] != false {
		t.Errorf("result.ok = %v; want false for unreachable host", result["ok"])
	}
	if result["error"] == nil || result["error"] == "" {
		t.Error("expected a non-empty error message")
	}
}

// TestModeStatus_TestConnection_ServerUnavailableSetsLikelyConnectionLimit
// proves the likely_connection_limit classification, which none of the
// other tests exercise (they hit "connection refused"/"not found", never
// a 502/503 NNTP response). Stands up a minimal raw TCP listener that
// writes a 502 greeting — the exact response most providers send when
// an account's connection limit is already in use — instead of using
// internal/nntp/nntptest's Scripted server, which always sends a
// successful 200/201 greeting (it's built for article-fetch scenarios,
// not greeting-rejection ones).
func TestModeStatus_TestConnection_ServerUnavailableSetsLikelyConnectionLimit(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed by test cleanup; nothing to serve
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte("502 Too many connections from your account\r\n"))
	}()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q): %v", portStr, err)
	}

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) {
		c.General.APIKey = testAPIKey
		c.Servers = []config.ServerConfig{{
			Name: "s1", Host: host, Port: port, Connections: 1, Timeout: 5,
		}}
	})
	s := New(Options{Config: cfg, App: &app.Application{}})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_connection&value=s1&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result := m["result"].(map[string]any)
	if result["ok"] != false {
		t.Errorf("result.ok = %v; want false", result["ok"])
	}
	if result["likely_connection_limit"] != true {
		t.Errorf("result.likely_connection_limit = %v; want true for a 502 greeting", result["likely_connection_limit"])
	}
}

func TestModeStatus_TestConnection_UnwiredApp(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Config: cfg})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_connection&value=s1&apikey="+testAPIKey)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 when App is not set", rr.Code)
	}
}

type diskSpeedSpyApp struct {
	apitest.NopApp
	mbPerSec float64
	err      error
}

func (a *diskSpeedSpyApp) TestDownloadDirWriteSpeedMBPerSec(ctx context.Context) (float64, error) {
	return a.mbPerSec, a.err
}

func TestModeStatus_TestDiskSpeed_Success(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Config: cfg, App: &diskSpeedSpyApp{mbPerSec: 210.4}})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_disk_speed&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result := m["result"].(map[string]any)
	if result["mb_per_sec"] != 210.4 {
		t.Errorf("result.mb_per_sec = %v; want 210.4", result["mb_per_sec"])
	}
}

func TestModeStatus_TestDiskSpeed_Error(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.APIKey = testAPIKey })
	s := New(Options{Config: cfg, App: &diskSpeedSpyApp{err: errors.New("permission denied")}})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=test_disk_speed&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	result := m["result"].(map[string]any)
	if result["ok"] != false {
		t.Errorf("result.ok = %v; want false on write error", result["ok"])
	}
}

func TestServer_StatusTestConnection_DirectCall(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	s := New(Options{Config: cfg, App: &app.Application{}})
	req := httptest.NewRequest(http.MethodGet, "/api?mode=status&name=test_connection&value=missing", nil)
	w := httptest.NewRecorder()
	s.statusTestConnection(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("statusTestConnection code = %d, want 200", w.Code)
	}
}
