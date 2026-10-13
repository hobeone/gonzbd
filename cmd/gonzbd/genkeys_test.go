package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadOrCreateConfig_PersistsGeneratedKeys pins that serve mode writes the
// keys Load generated into the file, and one-shot mode leaves the file alone.
func TestLoadOrCreateConfig_PersistsGeneratedKeys(t *testing.T) {
	const content = "# keep me\ngeneral:\n  api_key: \"\"\n  nzb_key: \"\"\n"
	for _, persist := range []bool{true, false} {
		path := filepath.Join(t.TempDir(), "gonzbd.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadOrCreateConfig(path, persist)
		if err != nil {
			t.Fatalf("persist=%v: %v", persist, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		g := cfg.GetGeneral()
		wrote := strings.Contains(string(got), "api_key: "+g.APIKey) &&
			strings.Contains(string(got), "nzb_key: "+g.NZBKey)
		if wrote != persist {
			t.Errorf("persist=%v: file holds the generated keys = %v; file:\n%s", persist, wrote, got)
		}
	}
}
