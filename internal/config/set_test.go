package config

import (
	"testing"
)

func TestConfig_Set(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}

	tests := []struct {
		name    string
		section string
		keyword string
		value   string
		wantErr bool
		check   func(t *testing.T, c *Config)
	}{
		{
			"set string (host)",
			"general", "host", "1.2.3.4",
			false,
			func(t *testing.T, c *Config) {
				if c.General.Host != "1.2.3.4" {
					t.Errorf("Host = %q, want 1.2.3.4", c.General.Host)
				}
			},
		},
		{
			"set int (port)",
			"general", "port", "9090",
			false,
			func(t *testing.T, c *Config) {
				if c.General.Port != 9090 {
					t.Errorf("Port = %d, want 9090", c.General.Port)
				}
			},
		},
		{
			"set bool (localhost_bypass)",
			"general", "localhost_bypass", "true",
			false,
			func(t *testing.T, c *Config) {
				if !c.General.LocalhostBypass {
					t.Error("LocalhostBypass is false, want true")
				}
			},
		},
		{
			"set ByteSize (bandwidth_max) numeric",
			"downloads", "bandwidth_max", "1048576",
			false,
			func(t *testing.T, c *Config) {
				if c.Downloads.BandwidthMax != 1024*1024 {
					t.Errorf("BandwidthMax = %v, want 1M", c.Downloads.BandwidthMax)
				}
			},
		},
		{
			"set ByteSize (bandwidth_max) human-readable",
			"downloads", "bandwidth_max", "10M",
			false,
			func(t *testing.T, c *Config) {
				if c.Downloads.BandwidthMax != 10*1024*1024 {
					t.Errorf("BandwidthMax = %v, want 10M", c.Downloads.BandwidthMax)
				}
			},
		},
		{
			"set ByteSize (bandwidth_max) unlimited",
			"downloads", "bandwidth_max", "unlimited",
			false,
			func(t *testing.T, c *Config) {
				if c.Downloads.BandwidthMax != 0 {
					t.Errorf("BandwidthMax = %v, want 0", c.Downloads.BandwidthMax)
				}
			},
		},
		{
			"set ByteSize invalid",
			"downloads", "bandwidth_max", "abc",
			true,
			nil,
		},
		{
			"set Percent (bandwidth_perc) valid",
			"downloads", "bandwidth_perc", "50",
			false,
			func(t *testing.T, c *Config) {
				if c.Downloads.BandwidthPerc != 50 {
					t.Errorf("BandwidthPerc = %d, want 50", c.Downloads.BandwidthPerc)
				}
			},
		},
		{
			"set Percent out of bounds",
			"downloads", "bandwidth_perc", "105",
			true,
			nil,
		},
		{
			"set Percent invalid",
			"downloads", "bandwidth_perc", "abc",
			true,
			nil,
		},
		{
			"set invalid section",
			"nosuch", "host", "val",
			true,
			nil,
		},
		{
			"set invalid keyword",
			"general", "nosuch", "val",
			true,
			nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := cfg.Set(tc.section, tc.keyword, tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

func TestConfig_SetSSLVerify(t *testing.T) {
	cfg, _ := Default()
	cfg.Servers = []ServerConfig{{Name: "test"}}

	// Note: Set currently doesn't support setting fields inside slice elements
	// by index/keyword via (section, keyword, value). It only supports
	// flat sections or the entire slice via JSON.
	// But let's verify if we can set it if it was a flat field (it's not currently,
	// but we added support in setFieldValue).
}
