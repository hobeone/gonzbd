package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "faultproxy.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfig_HappyPath(t *testing.T) {
	path := writeConfigFile(t, `
listen: "127.0.0.1:11190"
upstream:
  host: "news.example.com"
  port: 563
  ssl: true
rules:
  - message_ids: ["abc123@example"]
    action: drop
  - rate: 0.05
    action: corrupt
    corrupt_bytes: 4
  - message_ids: ["def456@example"]
    action: timeout
    timeout_after: 30s
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:11190" {
		t.Errorf("Listen = %q, want 127.0.0.1:11190", cfg.Listen)
	}
	if cfg.Upstream.Host != "news.example.com" || cfg.Upstream.Port != 563 || !cfg.Upstream.SSL {
		t.Errorf("Upstream = %+v, want host=news.example.com port=563 ssl=true", cfg.Upstream)
	}
	if len(cfg.Rules) != 3 {
		t.Fatalf("len(Rules) = %d, want 3", len(cfg.Rules))
	}
	if cfg.Rules[0].Action != "drop" || len(cfg.Rules[0].MessageIDs) != 1 || cfg.Rules[0].MessageIDs[0] != "abc123@example" {
		t.Errorf("Rules[0] = %+v", cfg.Rules[0])
	}
	if cfg.Rules[1].Action != "corrupt" || cfg.Rules[1].Rate != 0.05 || cfg.Rules[1].CorruptBytes != 4 {
		t.Errorf("Rules[1] = %+v", cfg.Rules[1])
	}
	if cfg.Rules[2].Action != "timeout" || cfg.Rules[2].TimeoutAfter != 30*time.Second {
		t.Errorf("Rules[2] = %+v", cfg.Rules[2])
	}
}

func TestLoadConfig_MissingListen(t *testing.T) {
	path := writeConfigFile(t, `
upstream:
  host: "news.example.com"
  port: 563
`)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for missing listen, got nil")
	}
}

func TestLoadConfig_MissingUpstreamHost(t *testing.T) {
	path := writeConfigFile(t, `
listen: "127.0.0.1:11190"
upstream:
  port: 563
`)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for missing upstream.host, got nil")
	}
}

func TestLoadConfig_UnknownAction(t *testing.T) {
	path := writeConfigFile(t, `
listen: "127.0.0.1:11190"
upstream:
  host: "news.example.com"
  port: 563
rules:
  - message_ids: ["abc@example"]
    action: explode
`)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for unknown action, got nil")
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	if _, err := LoadConfig("/nonexistent/faultproxy.yaml"); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
