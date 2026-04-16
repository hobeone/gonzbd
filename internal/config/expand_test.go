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
			got := expandHome(tc.in)
			if got != tc.want {
				t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestConfigExpandEnv(t *testing.T) {
	os.Setenv("TEST_PORT", "9999")
	os.Setenv("TEST_DIR", "/tmp/sabnzbd")
	defer os.Unsetenv("TEST_PORT")
	defer os.Unsetenv("TEST_DIR")

	yml := `
general:
  host: "127.0.0.1"
  port: ${TEST_PORT}
  download_dir: "$TEST_DIR/incomplete"
  complete_dir: "~/Downloads"
  api_key: "0123456789abcdef"
  nzb_key: "0123456789abcdef"
`
	cfg, err := decode(strings.NewReader(yml))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	cfg.ExpandPaths()

	if cfg.General.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.General.Port)
	}
	if cfg.General.DownloadDir != "/tmp/sabnzbd/incomplete" {
		t.Errorf("DownloadDir = %q, want /tmp/sabnzbd/incomplete", cfg.General.DownloadDir)
	}

	home, _ := os.UserHomeDir()
	wantComplete := filepath.Join(home, "Downloads")
	if cfg.General.CompleteDir != wantComplete {
		t.Errorf("CompleteDir = %q, want %q", cfg.General.CompleteDir, wantComplete)
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
