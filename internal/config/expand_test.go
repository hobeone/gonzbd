package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("HOME not set, skipping ~ expansion tests")
	}

	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"/abs/path", "/abs/path"},
		{"rel/path", "rel/path"},
		{"~", home},
		{"~/", home},
		{"~/Downloads", filepath.Join(home, "Downloads")},
		{"~user/path", "~user/path"}, // not supported
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := expandPath(tc.in)
			if got != tc.want {
				t.Errorf("expandPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestConfigExpandEnv(t *testing.T) {
	os.Setenv("TEST_DIR", "/tmp/gonzbd")
	defer os.Unsetenv("TEST_DIR")

	yml := `
general:
  host: "127.0.0.1"
  port: 9999
  download_dir: "$TEST_DIR/incomplete"
  complete_dir: "~/Downloads"
  api_key: "0123456789abcdef"
  nzb_key: "0123456789abcdef"
`
	cfg, _, err := decode(strings.NewReader(yml))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Before ExpandPaths, raw values should be preserved (no global expansion).
	if cfg.General.DownloadDir != "$TEST_DIR/incomplete" {
		t.Errorf("before ExpandPaths: DownloadDir = %q, want $TEST_DIR/incomplete (raw)", cfg.General.DownloadDir)
	}

	cfg.ExpandPaths()

	if cfg.General.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.General.Port)
	}
	// After ExpandPaths, env vars in path fields should be expanded.
	if cfg.General.DownloadDir != "/tmp/gonzbd/incomplete" {
		t.Errorf("DownloadDir = %q, want /tmp/gonzbd/incomplete", cfg.General.DownloadDir)
	}

	home, _ := os.UserHomeDir()
	wantComplete := filepath.Join(home, "Downloads")
	if cfg.General.CompleteDir != wantComplete {
		t.Errorf("CompleteDir = %q, want %q", cfg.General.CompleteDir, wantComplete)
	}
}

// TestConfigDollarInPassword verifies that passwords containing $ are
// not corrupted by env var expansion (the bug fixed by removing global
// os.ExpandEnv from decode).
func TestConfigDollarInPassword(t *testing.T) {
	yml := `
general:
  host: "127.0.0.1"
  port: 4289
  download_dir: "/tmp"
  complete_dir: "/tmp"
  api_key: "0123456789abcdef"
  nzb_key: "0123456789abcdef"
servers:
  - name: "test"
    host: "news.example.com"
    port: 563
    username: "user"
    password: "p$ss$w0rd"
    ssl: true
    connections: 1
`
	cfg, _, err := decode(strings.NewReader(yml))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Servers[0].Password != "p$ss$w0rd" {
		t.Errorf("password corrupted: got %q, want %q", cfg.Servers[0].Password, "p$ss$w0rd")
	}
}

func TestExpandPaths(t *testing.T) {
	home, _ := os.UserHomeDir()
	cfg := &Config{
		General: GeneralConfig{
			DownloadDir: "~/dl",
			CompleteDir: "~",
		},
		PostProc: PostProcConfig{
			Par2Command: "~/bin/par2",
		},
	}

	cfg.ExpandPaths()

	if cfg.General.DownloadDir != filepath.Join(home, "dl") {
		t.Errorf("General.DownloadDir not expanded: %q", cfg.General.DownloadDir)
	}
	if cfg.General.CompleteDir != home {
		t.Errorf("General.CompleteDir not expanded: %q", cfg.General.CompleteDir)
	}
	if cfg.PostProc.Par2Command != filepath.Join(home, "bin/par2") {
		t.Errorf("PostProc.Par2Command not expanded: %q", cfg.PostProc.Par2Command)
	}
}

func TestExpandPathsDirect(t *testing.T) {
	home, _ := os.UserHomeDir()
	g := &GeneralConfig{
		DownloadDir: "~/dl",
	}
	g.expandPaths()
	if g.DownloadDir != filepath.Join(home, "dl") {
		t.Errorf("expandPaths (GeneralConfig) failed: %q", g.DownloadDir)
	}

	p := &PostProcConfig{
		Par2Command: "~/bin/par2",
	}
	p.expandPaths()
	if p.Par2Command != filepath.Join(home, "bin/par2") {
		t.Errorf("expandPaths (PostProcConfig) failed: %q", p.Par2Command)
	}
}
