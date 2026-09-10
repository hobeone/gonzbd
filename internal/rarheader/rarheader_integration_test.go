//go:build integration

package rarheader

import (
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func requireTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found in PATH, skipping", name)
	}
	return p
}

func TestInspect_RAR3Fixtures(t *testing.T) {
	requireTool(t, "unrar")

	tests := []struct {
		file      string
		wantFiles []string
	}{
		{filepath.Join("testdata", "rar3-comment-plain.rar"), []string{"file1.txt", "file2.txt"}},
		{filepath.Join("testdata", "rar3-subdirs.rar"), []string{"file2.txt", "long fn.txt", "file.txt", "file1.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			info, err := Inspect(tt.file)
			if err != nil {
				t.Fatalf("Inspect(%s) error: %v", tt.file, err)
			}
			if info.Version != 3 {
				t.Errorf("Version = %d, want 3", info.Version)
			}
			if !slices.Equal(info.Filenames, tt.wantFiles) {
				t.Errorf("Filenames = %v, want %v", info.Filenames, tt.wantFiles)
			}
		})
	}
}
