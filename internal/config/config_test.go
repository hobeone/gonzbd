package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hobeone/gonzbd/internal/constants"
)

// marshalForCompare renders cfg to canonical YAML so we can compare two
// configs by their on-disk representation. This sidesteps the unexported
// sync.RWMutex inside Config which is unsafe to compare directly.
func marshalForCompare(t *testing.T, c *Config) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close encoder: %v", err)
	}
	return buf.Bytes()
}

func TestDefaultIsValid(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default config did not validate: %v", err)
	}
	if len(cfg.General.APIKey) != 16 {
		t.Errorf("APIKey length = %d, want 16", len(cfg.General.APIKey))
	}
	if cfg.General.APIKey == cfg.General.NZBKey {
		t.Errorf("APIKey and NZBKey are identical (%q); should be distinct random values", cfg.General.APIKey)
	}
	if cfg.Downloads.MinFreeSpace != ByteSize(1024*constants.MiB) {
		t.Errorf("MinFreeSpace = %d, want %d", cfg.Downloads.MinFreeSpace, 1024*constants.MiB)
	}
	if cfg.PostProc.Par2MaxPacketBodySize != 67108864 {
		t.Errorf("Par2MaxPacketBodySize = %d, want %d", cfg.PostProc.Par2MaxPacketBodySize, 67108864)
	}
	if cfg.PostProc.Par2MaxJunkScanBytes != 65536 {
		t.Errorf("Par2MaxJunkScanBytes = %d, want %d", cfg.PostProc.Par2MaxJunkScanBytes, 65536)
	}
}

func TestDefaultStrictSandbox(t *testing.T) {
	t.Run("native install defaults to strict", func(t *testing.T) {
		cfg, err := Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		if !cfg.PostProc.StrictSandbox {
			t.Errorf("StrictSandbox = false, want true when GONZBD_DOCKER is unset")
		}
	})

	t.Run("container image defaults to non-strict", func(t *testing.T) {
		t.Setenv("GONZBD_DOCKER", "1")
		cfg, err := Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		if cfg.PostProc.StrictSandbox {
			t.Errorf("StrictSandbox = true, want false when GONZBD_DOCKER=1 (bwrap is never installed in the image; strict mode would abort every extraction)")
		}
	})
}

func TestDefaultEnableTar(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	if !cfg.PostProc.EnableTar {
		t.Errorf("EnableTar = false, want true (matches SABnzbd's cfg.enable_tar() default)")
	}
}

func TestEnableTar_YAMLRoundTrip(t *testing.T) {
	t.Run("unset in YAML keeps default true", func(t *testing.T) {
		cfg, unknowns, err := decode(strings.NewReader("---\n"))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(unknowns) != 0 {
			t.Fatalf("unexpected unknown fields: %v", unknowns)
		}
		if !cfg.PostProc.EnableTar {
			t.Errorf("EnableTar = false, want true when omitted from YAML")
		}
	})

	t.Run("explicit false is honored", func(t *testing.T) {
		cfg, _, err := decode(strings.NewReader("postproc:\n  enable_tar: false\n"))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cfg.PostProc.EnableTar {
			t.Errorf("EnableTar = true, want false when explicitly set to false in YAML")
		}
	})

	t.Run("round-trips through Save/Load", func(t *testing.T) {
		original, err := Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		original.PostProc.EnableTar = false
		path := filepath.Join(t.TempDir(), "gonzbd.yaml")
		if err := original.Save(path); err != nil {
			t.Fatalf("Save: %v", err)
		}
		loaded, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if loaded.PostProc.EnableTar {
			t.Errorf("EnableTar = true after round-trip, want false")
		}
	})
}

func TestRoundTripDefault(t *testing.T) {
	original, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	path := filepath.Join(t.TempDir(), "gonzbd.yaml")
	if err := original.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := marshalForCompare(t, original)
	got := marshalForCompare(t, loaded)
	if !bytes.Equal(want, got) {
		t.Fatalf("round-trip diverged:\n--- original ---\n%s\n--- loaded ---\n%s", want, got)
	}
}

func TestRoundTripFixture(t *testing.T) {
	// Locate the test fixture relative to this test file by walking up
	// to the module root and then into test/fixtures.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// cwd is .../internal/config; module root is two levels up.
	fixture := filepath.Join(cwd, "..", "..", "test", "fixtures", "gonzbd.yaml")
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("fixture not present at %s: %v", fixture, err)
	}

	cfg, err := Load(fixture)
	if err != nil {
		t.Fatalf("Load fixture: %v", err)
	}

	// Save and reload — must be idempotent.
	out := filepath.Join(t.TempDir(), "out.yaml")
	if err := cfg.Save(out); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(out)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if !bytes.Equal(marshalForCompare(t, cfg), marshalForCompare(t, reloaded)) {
		t.Fatalf("fixture round-trip diverged")
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		in      string
		want    ByteSize
		wantErr bool
	}{
		{"", 0, false},
		{"unlimited", 0, false},
		{"UNLIMITED", 0, false},
		{"0", 0, false},
		{"1024", 1024, false},
		{"1K", 1024, false},
		{"2k", 2048, false},
		{"1M", 1024 * 1024, false},
		{"1.5G", 1024*1024*1024 + 512*1024*1024, false},
		{"1T", 1024 * 1024 * 1024 * 1024, false},
		{"-5", 0, true},
		{"-1M", 0, true},
		{"abc", 0, true},
		{"M", 0, true},
		{"1X", 0, true}, // unknown suffix becomes part of integer parse, fails
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseByteSize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseByteSize(%q) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseByteSize(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestByteSizeYAMLRoundTrip(t *testing.T) {
	type holder struct {
		B ByteSize `yaml:"b"`
	}
	tests := []struct {
		in  string
		out string
	}{
		{"b: 1024\n", "b: 1K\n"},
		{"b: 1M\n", "b: 1M\n"},
		{"b: 1.5G\n", "b: 1536M\n"}, // canonicalized to MiB-aligned form
		{"b: '0'\n", "b: \"0\"\n"},
		{"b: unlimited\n", "b: \"0\"\n"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			var h holder
			if err := yaml.Unmarshal([]byte(tc.in), &h); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out, err := yaml.Marshal(h)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(out) != tc.out {
				t.Fatalf("round-trip:\n  in:  %q\n  got: %q\n  want: %q", tc.in, string(out), tc.out)
			}
		})
	}
}

func TestUnknownFieldsWarn(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	out := marshalForCompare(t, cfg)
	// Introduce an unknown field (e.g. from a removed or misspelled key).
	corrupted := bytes.Replace(out, []byte("port:"), []byte("portt:"), 1)

	// Unknown fields must not cause a fatal error — the config loads successfully.
	result, unknowns, decErr := decode(bytes.NewReader(corrupted))
	if decErr != nil {
		t.Fatalf("decode returned fatal error for unknown field: %v", decErr)
	}
	if result == nil {
		t.Fatal("decode returned nil config for unknown field")
	}
	// The unknown field must be reported as a warning, not silently dropped.
	if len(unknowns) == 0 {
		t.Fatal("decode should return a warning for unknown field 'portt'")
	}
	if !strings.Contains(unknowns[0], "portt") {
		t.Errorf("warning %q should mention the unknown field name", unknowns[0])
	}
}

func TestTypeErrorsRemainFatal(t *testing.T) {
	// A wrong-type error (string where int expected) must still be fatal,
	// even though unknown-field errors are demoted to warnings.
	yml := `
servers:
  - host: "news.example.com"
    port: "not-a-number"
    connections: 4
`
	_, _, err := decode(strings.NewReader(yml))
	if err == nil {
		t.Fatal("decode should return fatal error for type mismatch (string into int)")
	}
}

func TestValidateRejectsBadInputs(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{
			"empty host",
			func(c *Config) { c.General.Host = "" },
			"host",
		},
		{
			"port out of range",
			func(c *Config) { c.General.Port = 70000 },
			"port",
		},
		{
			"https port set without cert",
			func(c *Config) {
				c.General.HTTPSPort = 8443
				c.General.HTTPSCert = ""
				c.General.HTTPSKey = ""
			},
			"https_cert",
		},
		{
			"bad api_key",
			func(c *Config) { c.General.APIKey = "not-hex" },
			"api_key",
		},
		{
			"duplicate category names",
			func(c *Config) {
				c.Categories = append(c.Categories, c.Categories[0])
			},
			"category name",
		},
		{
			"invalid pp",
			func(c *Config) {
				c.Categories[0].PP = 9
			},
			"pp",
		},
		{
			"bad permissions (non-octal char)",
			func(c *Config) { c.PostProc.Permissions = "7a5" },
			"permissions",
		},
		{
			"bad permissions (too short)",
			func(c *Config) { c.PostProc.Permissions = "75" },
			"permissions",
		},
		{
			"bad permissions (too long)",
			func(c *Config) { c.PostProc.Permissions = "17550" },
			"permissions",
		},
		{
			"extra_unrar_params non-flag",
			func(c *Config) { c.PostProc.ExtraUnrarParams = "-sl100000 badarg" },
			"extra_unrar_params",
		},
		{
			"extra_unrar_params disallowed flag",
			func(c *Config) { c.PostProc.ExtraUnrarParams = "-df -mlp" },
			"not allowed",
		},
		{
			"extra_par2_params non-flag",
			func(c *Config) { c.PostProc.ExtraPar2Params = "notaflag" },
			"extra_par2_params",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Default()
			if err != nil {
				t.Fatalf("Default(): %v", err)
			}
			tc.mutate(cfg)
			err = cfg.Validate()
			if err == nil {
				t.Fatalf("expected validation error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidateAcceptsAValidServer(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.Servers = []ServerConfig{
		{
			Name:               "primary",
			Host:               "news.example.com",
			Port:               563,
			Connections:        8,
			SSL:                true,
			SSLVerify:          SSLVerifyHostname,
			Priority:           0,
			Required:           true,
			Timeout:            60,
			PipeliningRequests: 2,
			Enable:             true,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validation: %v", err)
	}
}

func TestSaveAtomicReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gonzbd.yaml")

	first, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	if err := first.Save(path); err != nil {
		t.Fatalf("first save: %v", err)
	}

	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read first: %v", err)
	}

	// Modify and save again; the file at `path` must be the new version
	// in its entirety, with no leftover temp files in the directory.
	first.General.Port = 9090
	if err := first.Save(path); err != nil {
		t.Fatalf("second save: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if bytes.Equal(original, got) {
		t.Fatal("second save produced identical bytes; expected port change to take effect")
	}
	if !bytes.Contains(got, []byte("port: 9090")) {
		t.Errorf("expected port: 9090 in output, got:\n%s", got)
	}

	// No temp files should remain.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestPercentRange(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"50", false},
		{"0", false},
		{"100", false},
		{"-1", true},
		{"101", true},
		{"abc", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			var p Percent
			err := yaml.Unmarshal([]byte(tc.in), &p)
			if tc.wantErr && err == nil {
				t.Fatalf("Percent(%q) = %d, want error", tc.in, p)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Percent(%q): unexpected error: %v", tc.in, err)
			}
		})
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		want       slog.Level
		wantErr    bool
		wantErrMsg string
	}{
		{"debug", "debug", slog.LevelDebug, false, ""},
		{"DEBUG uppercase", "DEBUG", slog.LevelDebug, false, ""},
		{"Debug mixed case", "Debug", slog.LevelDebug, false, ""},
		{"info", "info", slog.LevelInfo, false, ""},
		{"INFO uppercase", "INFO", slog.LevelInfo, false, ""},
		{"Info mixed case", "Info", slog.LevelInfo, false, ""},
		{"warn", "warn", slog.LevelWarn, false, ""},
		{"WARN uppercase", "WARN", slog.LevelWarn, false, ""},
		{"Warn mixed case", "Warn", slog.LevelWarn, false, ""},
		{"error", "error", slog.LevelError, false, ""},
		{"ERROR uppercase", "ERROR", slog.LevelError, false, ""},
		{"Error mixed case", "Error", slog.LevelError, false, ""},
		{"off", "off", LevelOff, false, ""},
		{"OFF uppercase", "OFF", LevelOff, false, ""},
		{"empty defaults to info", "", slog.LevelInfo, false, ""},
		{"invalid level", "invalid", 0, true, "invalid log level"},
		{"trace invalid", "trace", 0, true, "invalid log level"},
		{"bad input", "foobar", 0, true, "invalid log level"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &GeneralConfig{LogLevel: tt.input}
			got, err := g.ParseLogLevel()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseLogLevel(%q) = %v, want error", tt.input, got)
				}
				if !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLogLevel(%q): unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseLogLevels(t *testing.T) {
	t.Run("nil map returns nil", func(t *testing.T) {
		g := &GeneralConfig{}
		got, err := g.ParseLogLevels()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("valid map", func(t *testing.T) {
		g := &GeneralConfig{
			LogLevels: map[string]string{
				"api":        "warn",
				"downloader": "debug",
				"nntp":       "error",
			},
		}
		got, err := g.ParseLogLevels()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["api"] != slog.LevelWarn {
			t.Errorf("api = %v, want %v", got["api"], slog.LevelWarn)
		}
		if got["downloader"] != slog.LevelDebug {
			t.Errorf("downloader = %v, want %v", got["downloader"], slog.LevelDebug)
		}
		if got["nntp"] != slog.LevelError {
			t.Errorf("nntp = %v, want %v", got["nntp"], slog.LevelError)
		}
	})

	t.Run("invalid level rejects", func(t *testing.T) {
		g := &GeneralConfig{
			LogLevels: map[string]string{
				"api": "bogus",
			},
		}
		_, err := g.ParseLogLevels()
		if err == nil {
			t.Fatal("expected error for invalid level")
		}
		if !strings.Contains(err.Error(), "invalid log level") {
			t.Errorf("error %q does not mention 'invalid log level'", err.Error())
		}
	})
}

// M17: Empty file should return defaults (not panic or error).
func TestLoad_EmptyFileReturnsDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("write empty file: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load empty file: %v", err)
	}
	if len(cfg.General.APIKey) != 16 {
		t.Errorf("APIKey length = %d, want 16", len(cfg.General.APIKey))
	}
	if cfg.General.Port == 0 {
		t.Error("Port should have a default value, got 0")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config from empty file should validate: %v", err)
	}
}

// M18: Empty post-proc paths should validate OK (auto-detect via PATH).
func TestValidate_PostProcPaths(t *testing.T) {
	t.Parallel()
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	// Empty all post-processing paths — should be accepted (auto-detect via PATH).
	cfg.PostProc.Par2Command = ""
	cfg.PostProc.UnrarCommand = ""
	cfg.PostProc.SevenzCommand = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty PostProc paths should validate OK (auto-detect via PATH): %v", err)
	}
}

// M19: Verify that os.ExpandEnv is only applied to path fields, not raw YAML.
func TestExpandPath_EnvVarInPathOnly(t *testing.T) {
	// Cannot use t.Parallel() because t.Setenv mutates the process environment.
	t.Setenv("GONZBD_TEST_EXPAND", "/resolved")
	got := expandPath("$GONZBD_TEST_EXPAND/downloads")
	want := "/resolved/downloads"
	if got != want {
		t.Errorf("expandPath($GONZBD_TEST_EXPAND/downloads) = %q, want %q", got, want)
	}

	// Verify that raw YAML decode does NOT expand env vars.
	// A password containing a $VAR literal should be preserved verbatim.
	t.Setenv("SECRET", "leaked")
	yamlData := `
general:
  host: 0.0.0.0
  port: 8080
  api_key: "0123456789abcdef"
  nzb_key: "fedcba9876543210"
  download_dir: "/tmp/dl"
  complete_dir: "/tmp/complete"
servers:
  - name: test
    host: news.example.com
    port: 563
    username: user
    password: "pass$SECRET"
    connections: 8
    ssl: true
    ssl_verify: 2
    timeout: 60
    pipelining_requests: 2
    enable: true
`
	cfg, _, err := decode(strings.NewReader(yamlData))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The password should NOT have $SECRET expanded.
	if cfg.Servers[0].Password != "pass$SECRET" {
		t.Errorf("password = %q, want %q (env var should NOT be expanded in non-path fields)",
			cfg.Servers[0].Password, "pass$SECRET")
	}
}

func TestConfig_ValueGetters_Isolation(t *testing.T) {
	cfg := &Config{
		General: GeneralConfig{
			Host:        "localhost",
			LocalRanges: []string{"192.168.1.0/24"},
			LogLevels:   map[string]string{"downloader": "debug"},
		},
		Downloads: DownloadConfig{
			MaxArtTries: 3,
			CleanupList: []string{"nfo"},
		},
		PostProc: PostProcConfig{
			CleanupExtensions: []string{"txt"},
		},
		Servers: []ServerConfig{
			{Name: "server1", Host: "news.example.com"},
		},
		Categories: []CategoryConfig{
			{Name: "tv"},
		},
		Notifications: NotificationConfig{
			Email: EmailNotificationConfig{To: []string{"admin@example.com"}},
		},
	}

	// 1. Test GetGeneral isolation
	gen := cfg.GetGeneral()
	gen.Host = "mutated"
	gen.LocalRanges[0] = "10.0.0.0/8"
	gen.LogLevels["downloader"] = "error"
	if cfg.General.Host != "localhost" {
		t.Errorf("GetGeneral().Host mutated")
	}
	if cfg.General.LocalRanges[0] != "192.168.1.0/24" {
		t.Errorf("GetGeneral().LocalRanges mutated")
	}
	if cfg.General.LogLevels["downloader"] != "debug" {
		t.Errorf("GetGeneral().LogLevels mutated")
	}

	// 2. Test GetDownloads isolation
	dl := cfg.GetDownloads()
	dl.MaxArtTries = 99
	dl.CleanupList[0] = "mutated"
	if cfg.Downloads.MaxArtTries != 3 || cfg.Downloads.CleanupList[0] != "nfo" {
		t.Errorf("GetDownloads() mutated live config")
	}

	// 3. Test GetPostProc isolation
	pp := cfg.GetPostProc()
	pp.CleanupExtensions[0] = "mutated"
	if cfg.PostProc.CleanupExtensions[0] != "txt" {
		t.Errorf("GetPostProc() mutated live config")
	}

	// 4. Test GetServers isolation
	srvs := cfg.GetServers()
	srvs[0].Name = "mutated"
	if cfg.Servers[0].Name != "server1" {
		t.Errorf("GetServers() mutated live config")
	}

	// 5. Test GetCategories isolation
	cats := cfg.GetCategories()
	cats[0].Name = "mutated"
	if cfg.Categories[0].Name != "tv" {
		t.Errorf("GetCategories() mutated live config")
	}

	// 6. Test GetNotifications isolation
	notif := cfg.GetNotifications()
	notif.Email.To[0] = "mutated"
	if cfg.Notifications.Email.To[0] != "admin@example.com" {
		t.Errorf("GetNotifications() mutated live config")
	}

	// 7. Test IngestSnapshot isolation
	snap := cfg.IngestSnapshot()
	snap.Downloads.CleanupList[0] = "mutated"
	snap.Categories[0].Name = "mutated"
	if cfg.Downloads.CleanupList[0] != "nfo" || cfg.Categories[0].Name != "tv" {
		t.Errorf("IngestSnapshot() mutated live config")
	}

	// 8. Test Snapshot isolation
	full := cfg.Snapshot()
	full.General.LocalRanges[0] = "mutated"
	if cfg.General.LocalRanges[0] != "192.168.1.0/24" {
		t.Errorf("Snapshot() mutated live config")
	}
}
