package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

func TestModeStatus_Config_RedactsSecrets(t *testing.T) {
	t.Parallel()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.With(func(c *config.Config) {
		c.General.APIKey = testAPIKey
		c.Servers = []config.ServerConfig{{Name: "s1", Host: "news.example.com", Password: "hunter2"}}
	})
	s := New(Options{Config: cfg})

	rr := apiGet(t, s.Handler(), "/api?mode=status&name=config&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	m := decodeJSON(t, rr)
	cfgOut, ok := m["config"].(map[string]any)
	if !ok {
		t.Fatalf("expected config object, got %v", m)
	}
	general, ok := cfgOut["general"].(map[string]any)
	if !ok {
		t.Fatalf("expected general section, got %v", cfgOut)
	}
	if general["api_key"] != config.RedactedPlaceholder {
		t.Errorf("general.api_key = %v; want redacted", general["api_key"])
	}
	servers, ok := cfgOut["servers"].([]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("expected 1 server, got %v", cfgOut["servers"])
	}
	server := servers[0].(map[string]any)
	if server["password"] != config.RedactedPlaceholder {
		t.Errorf("servers[0].password = %v; want redacted", server["password"])
	}
	if server["host"] != "news.example.com" {
		t.Errorf("servers[0].host = %v; want unchanged", server["host"])
	}
}

func TestModeStatus_Config_NoConfigWired(t *testing.T) {
	t.Parallel()
	s := New(Options{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api?mode=status&name=config", nil)
	s.statusConfig(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 when config not wired", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "config not wired") {
		t.Errorf("body = %q; want to contain 'config not wired'", rr.Body.String())
	}
}
