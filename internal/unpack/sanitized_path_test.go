package unpack_test

import (
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

func TestNewSanitizedPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       string
		oneFolder bool
		wantRel   string
		wantErr   string
	}{
		{"normal relative path", "folder/file.txt", false, "folder/file.txt", ""},
		{"windows separators", `folder\sub\file.txt`, false, "folder/sub/file.txt", ""},
		{"one folder mode", "folder/sub/file.txt", true, "file.txt", ""},
		{"dotdot escape rejected", "../../etc/passwd", false, "", "escapes output directory"},
		{"null byte rejected", "folder/\x00file.txt", false, "", "contains null byte"},
		{"empty after clean", ".", false, "", "empty after sanitization"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := unpack.NewSanitizedPath(tt.raw, tt.oneFolder)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("NewSanitizedPath(%q, %v) expected error containing %q, got nil", tt.raw, tt.oneFolder, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewSanitizedPath(%q, %v) error = %v, want error containing %q", tt.raw, tt.oneFolder, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewSanitizedPath(%q, %v) unexpected error: %v", tt.raw, tt.oneFolder, err)
			}
			if got.Rel() != tt.wantRel {
				t.Errorf("Rel() = %q, want %q", got.Rel(), tt.wantRel)
			}
			if got.Abs("/tmp/out") != "/tmp/out/"+tt.wantRel {
				t.Errorf("Abs() = %q, want %q", got.Abs("/tmp/out"), "/tmp/out/"+tt.wantRel)
			}
		})
	}
}
