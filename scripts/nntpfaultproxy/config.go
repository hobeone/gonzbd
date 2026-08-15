package main

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// UpstreamConfig describes the real NNTP server the proxy connects to on
// behalf of each downstream (gonzbd) connection.
type UpstreamConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// SSL enables TLS when dialing upstream.
	SSL bool `yaml:"ssl"`
	// SSLInsecure skips upstream certificate verification. Only ever
	// appropriate for a local validation tool talking to a server you
	// already trust by host/IP.
	SSLInsecure bool `yaml:"ssl_insecure"`
}

// Rule selects which BODY/STAT requests get a fault applied, and what kind.
// Rules are evaluated in order; the first match wins.
type Rule struct {
	// MessageIDs is an exact-match allowlist (without angle brackets). When
	// non-empty, Rate is ignored and only these message-IDs can match.
	MessageIDs []string `yaml:"message_ids,omitempty"`
	// Rate, in (0,1], makes the rule match that fraction of requests when
	// MessageIDs is empty (probabilistic mode).
	Rate float64 `yaml:"rate,omitempty"`
	// Action is one of "drop", "corrupt", "timeout".
	Action string `yaml:"action"`
	// CorruptBytes is the number of bytes to flip for action "corrupt".
	// Defaults to 1 if unset.
	CorruptBytes int `yaml:"corrupt_bytes,omitempty"`
	// TimeoutAfter is how long action "timeout" holds the connection open
	// before closing it without responding. Defaults to 60s if unset.
	TimeoutAfter time.Duration `yaml:"timeout_after,omitempty"`
}

// Config is the top-level fault proxy configuration file.
type Config struct {
	// Listen is the local address the proxy accepts downstream (gonzbd)
	// connections on, e.g. "127.0.0.1:11190".
	Listen   string         `yaml:"listen"`
	Upstream UpstreamConfig `yaml:"upstream"`
	Rules    []Rule         `yaml:"rules"`
}

// LoadConfig reads and validates a fault proxy YAML config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: user-supplied config path is the whole point
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Listen == "" {
		return nil, fmt.Errorf("config %s: listen is required", path)
	}
	if cfg.Upstream.Host == "" {
		return nil, fmt.Errorf("config %s: upstream.host is required", path)
	}
	if cfg.Upstream.Port == 0 {
		return nil, fmt.Errorf("config %s: upstream.port is required", path)
	}
	for i, r := range cfg.Rules {
		switch r.Action {
		case "drop", "corrupt", "timeout":
		default:
			return nil, fmt.Errorf("config %s: rule %d: unknown action %q", path, i, r.Action)
		}
	}
	return &cfg, nil
}
