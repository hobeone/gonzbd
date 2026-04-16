package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
}

func TestRoundTripDefault(t *testing.T) {
	original, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	path := filepath.Join(t.TempDir(), "sabnzbd.yaml")
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
	fixture := filepath.Join(cwd, "..", "..", "test", "fixtures", "sabnzbd.yaml")
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
			got, err := parseByteSize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseByteSize(%q) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseByteSize(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
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

func TestKnownFieldsRejectsTypos(t *testing.T) {
	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	out := marshalForCompare(t, cfg)
	corrupted := bytes.Replace(out, []byte("port:"), []byte("portt:"), 1)

	_, err = decode(bytes.NewReader(corrupted))
	if err == nil {
		t.Fatal("decode should reject unknown field 'portt'")
	}
	if !strings.Contains(err.Error(), "portt") {
		t.Errorf("error %q should mention the unknown field name", err.Error())
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
			"min_free_space_cleanup < min_free_space",
			func(c *Config) {
				c.Downloads.MinFreeSpace = 10 * 1024 * 1024
				c.Downloads.MinFreeSpaceCleanup = 5 * 1024 * 1024
			},
			"min_free_space_cleanup",
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
	path := filepath.Join(dir, "sabnzbd.yaml")

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
