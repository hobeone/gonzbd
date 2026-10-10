package unpack_test

import (
	"log/slog"
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

// MemberPath is the name check both RAR extraction loops apply to every
// member. rarengine rewrites "../" out of names before they reach it, so a
// fixture cannot exercise it; it is driven with the names directly.
func TestMemberPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		oneFolder bool
		wantRel   string // "" means refused
	}{
		{"../x", false, ""},
		{"a/../../x", false, ""},
		{`..\..\etc\passwd`, false, ""},
		{"a/b\x00c", false, ""},
		{"", false, ""},
		{".", false, ""},
		{"/etc/passwd", false, "etc/passwd"},
		{`dir\sub\file.mkv`, false, "dir/sub/file.mkv"},
		{"a/./b//c.txt", false, "a/b/c.txt"},
		{"../x", true, "x"},
		{"movie.mkv", false, "movie.mkv"},
	} {
		var lines []string
		var logs strings.Builder
		opts := unpack.Options{OneFolder: tc.oneFolder, OnLine: func(l string) { lines = append(lines, l) }}
		sp, ok := unpack.MemberPath(tc.name, opts, slog.New(slog.NewTextHandler(&logs, nil)))
		if tc.wantRel == "" {
			if ok {
				t.Errorf("MemberPath(%q) accepted as %q, want refused", tc.name, sp.Rel())
			}
			if len(lines) != 1 || lines[0] != "Skipping bad path: "+tc.name {
				t.Errorf("MemberPath(%q) reported %v", tc.name, lines)
			}
			if !strings.Contains(logs.String(), "skipping entry with bad path") {
				t.Errorf("MemberPath(%q) refusal not logged: %s", tc.name, logs.String())
			}
			continue
		}
		if !ok || sp.Rel() != tc.wantRel {
			t.Errorf("MemberPath(%q, oneFolder=%v) = %q, %v; want %q", tc.name, tc.oneFolder, sp.Rel(), ok, tc.wantRel)
		}
		if len(lines) != 0 {
			t.Errorf("MemberPath(%q) reported %v for an accepted name", tc.name, lines)
		}
	}
}
