package config

import (
	"reflect"
	"strings"
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
			"set Percent (bandwidth_perc) boundary 0",
			"downloads", "bandwidth_perc", "0",
			false,
			func(t *testing.T, c *Config) {
				if c.Downloads.BandwidthPerc != 0 {
					t.Errorf("BandwidthPerc = %d, want 0", c.Downloads.BandwidthPerc)
				}
			},
		},
		{
			"set Percent (bandwidth_perc) boundary 100",
			"downloads", "bandwidth_perc", "100",
			false,
			func(t *testing.T, c *Config) {
				if c.Downloads.BandwidthPerc != 100 {
					t.Errorf("BandwidthPerc = %d, want 100", c.Downloads.BandwidthPerc)
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
		{
			"set string slice (cleanup_list)",
			"downloads", "cleanup_list", `["a", "b", "c"]`,
			false,
			func(t *testing.T, c *Config) {
				want := []string{"a", "b", "c"}
				if len(c.Downloads.CleanupList) != 3 {
					t.Fatalf("len(CleanupList) = %d, want 3", len(c.Downloads.CleanupList))
				}
				for i, v := range c.Downloads.CleanupList {
					if v != want[i] {
						t.Errorf("CleanupList[%d] = %q, want %q", i, v, want[i])
					}
				}
			},
		},
		{
			"set empty string slice",
			"downloads", "cleanup_list", `[]`,
			false,
			func(t *testing.T, c *Config) {
				if len(c.Downloads.CleanupList) != 0 {
					t.Fatalf("len(CleanupList) = %d, want 0", len(c.Downloads.CleanupList))
				}
			},
		},
		{
			"set string slice invalid JSON",
			"downloads", "cleanup_list", `["a", "b", "c`,
			true,
			nil,
		},
		// The cases below verify that Set() correctly converts and persists typed
		// values (ByteSize, int, bool, string). Keyword existence is enforced
		// exhaustively in ui_contract_test.go — see TestUIKeywordsAreValidConfigTags.
		{
			"set ByteSize (min_free_space)",
			"downloads", "min_free_space", "5G",
			false,
			func(t *testing.T, c *Config) {
				if c.Downloads.MinFreeSpace != 5*1024*1024*1024 {
					t.Errorf("MinFreeSpace = %v, want 5G", c.Downloads.MinFreeSpace)
				}
			},
		},
		{
			"set ByteSize (write_cache_size)",
			"downloads", "write_cache_size", "500M",
			false,
			func(t *testing.T, c *Config) {
				if c.Downloads.WriteCacheSize != 500*1024*1024 {
					t.Errorf("WriteCacheSize = %v, want 500M", c.Downloads.WriteCacheSize)
				}
			},
		},
		{
			"set int (max_art_tries)",
			"downloads", "max_art_tries", "5",
			false,
			func(t *testing.T, c *Config) {
				if c.Downloads.MaxArtTries != 5 {
					t.Errorf("MaxArtTries = %d, want 5", c.Downloads.MaxArtTries)
				}
			},
		},
		{
			"set bool (pre_check)",
			"downloads", "pre_check", "true",
			false,
			func(t *testing.T, c *Config) {
				if !c.Downloads.PreCheck {
					t.Error("PreCheck is false, want true")
				}
			},
		},
		{
			"set string (replace_illegal_with)",
			"downloads", "replace_illegal_with", ".",
			false,
			func(t *testing.T, c *Config) {
				if c.Downloads.ReplaceIllegalWith != "." {
					t.Errorf("ReplaceIllegalWith = %q, want .", c.Downloads.ReplaceIllegalWith)
				}
			},
		},
		{
			"set string (replace_spaces_with)",
			"downloads", "replace_spaces_with", "_",
			false,
			func(t *testing.T, c *Config) {
				if c.Downloads.ReplaceSpacesWith != "_" {
					t.Errorf("ReplaceSpacesWith = %q, want _", c.Downloads.ReplaceSpacesWith)
				}
			},
		},
		{
			"set bool (strip_diacritics)",
			"downloads", "strip_diacritics", "true",
			false,
			func(t *testing.T, c *Config) {
				if !c.Downloads.StripDiacritics {
					t.Error("StripDiacritics is false, want true")
				}
			},
		},
		{
			"set slice (servers)",
			"servers", "", `[{"name": "test-srv", "host": "news.example.com", "port": 119, "connections": 4, "enable": true, "timeout": 60, "pipelining_requests": 2}]`,
			false,
			func(t *testing.T, c *Config) {
				if len(c.Servers) != 1 {
					t.Fatalf("len(Servers) = %d, want 1", len(c.Servers))
				}
				if c.Servers[0].Name != "test-srv" {
					t.Errorf("Server Name = %q, want test-srv", c.Servers[0].Name)
				}
			},
		},
		{
			"set slice (servers) invalid JSON",
			"servers", "", `[invalid-json`,
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

// TestConfig_SetValidationRollback verifies that Set rejects changes
// that would make the config invalid and rolls back to the previous value.
func TestConfig_SetValidationRollback(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}

	// Capture the valid port before mutation.
	origPort := cfg.General.Port

	// Attempt to set port to an invalid value (negative).
	err = cfg.Set("general", "port", "-5")
	if err == nil {
		t.Fatal("expected validation error for negative port, got nil")
	}
	if !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("error should mention validation failed: %v", err)
	}

	// Port should be rolled back to the original value.
	if cfg.General.Port != origPort {
		t.Errorf("Port = %d after rollback, want %d", cfg.General.Port, origPort)
	}

	// A valid change should still work.
	if err := cfg.Set("general", "port", "9999"); err != nil {
		t.Fatalf("valid Set failed: %v", err)
	}
	if cfg.General.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.General.Port)
	}
}

func TestSetUnexportedHelpersDirect(t *testing.T) {
	type dummy struct {
		Name        string    `json:"name"`
		Port        int       `yaml:"port"`
		ByteSizeVal ByteSize  `json:"bytes"`
		PercentVal  Percent   `json:"percent"`
		SSLVal      SSLVerify `json:"ssl"`
		Bypass      bool      `json:"bypass"`
		SliceVal    []string  `json:"slice"`
	}

	t.Run("findFieldByTag", func(t *testing.T) {
		d := dummy{}
		v := reflect.ValueOf(&d).Elem()

		f1 := findFieldByTag(v, "name")
		if !f1.IsValid() || f1.Kind() != reflect.String {
			t.Error("expected to find field 'name'")
		}

		f2 := findFieldByTag(v, "port")
		if !f2.IsValid() || f2.Kind() != reflect.Int {
			t.Error("expected to find field 'port'")
		}

		fNone := findFieldByTag(v, "nonexistent")
		if fNone.IsValid() {
			t.Error("expected not to find 'nonexistent'")
		}
	})

	t.Run("setFieldValue", func(t *testing.T) {
		d := dummy{}
		v := reflect.ValueOf(&d).Elem()

		// 1. String
		f := findFieldByTag(v, "name")
		if err := setFieldValue(f, "hello"); err != nil {
			t.Fatalf("set string: %v", err)
		}
		if d.Name != "hello" {
			t.Errorf("expected 'hello', got %q", d.Name)
		}

		// 2. Int
		f = findFieldByTag(v, "port")
		if err := setFieldValue(f, "1234"); err != nil {
			t.Fatalf("set int: %v", err)
		}
		if d.Port != 1234 {
			t.Errorf("expected 1234, got %d", d.Port)
		}

		// 3. ByteSize
		f = findFieldByTag(v, "bytes")
		if err := setFieldValue(f, "5M"); err != nil {
			t.Fatalf("set bytes: %v", err)
		}
		if d.ByteSizeVal != 5*1024*1024 {
			t.Errorf("expected 5M, got %v", d.ByteSizeVal)
		}

		// 4. Percent
		f = findFieldByTag(v, "percent")
		if err := setFieldValue(f, "75"); err != nil {
			t.Fatalf("set percent: %v", err)
		}
		if d.PercentVal != 75 {
			t.Errorf("expected 75, got %v", d.PercentVal)
		}

		// 5. SSLVerify
		f = findFieldByTag(v, "ssl")
		if err := setFieldValue(f, "2"); err != nil {
			t.Fatalf("set ssl: %v", err)
		}
		if d.SSLVal != 2 {
			t.Errorf("expected 2, got %v", d.SSLVal)
		}

		// 6. Bool
		f = findFieldByTag(v, "bypass")
		if err := setFieldValue(f, "true"); err != nil {
			t.Fatalf("set bool: %v", err)
		}
		if !d.Bypass {
			t.Error("expected true")
		}

		// 7. Slice
		f = findFieldByTag(v, "slice")
		if err := setFieldValue(f, `["x", "y"]`); err != nil {
			t.Fatalf("set slice: %v", err)
		}
		if len(d.SliceVal) != 2 || d.SliceVal[0] != "x" || d.SliceVal[1] != "y" {
			t.Errorf("unexpected slice value: %v", d.SliceVal)
		}
	})
}
