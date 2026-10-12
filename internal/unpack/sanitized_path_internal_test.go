package unpack

import (
	"log/slog"
	"strings"
	"testing"
)

// memberPath is the name check both RAR extraction loops apply to every
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
		opts := Options{OneFolder: tc.oneFolder, OnLine: func(l string) { lines = append(lines, l) }}
		sp, ok := memberPath(tc.name, opts, slog.New(slog.NewTextHandler(&logs, nil)))
		if tc.wantRel == "" {
			if ok {
				t.Errorf("memberPath(%q) accepted as %q, want refused", tc.name, sp.Rel())
			}
			if len(lines) != 1 || lines[0] != "Skipping bad path: "+tc.name {
				t.Errorf("memberPath(%q) reported %v", tc.name, lines)
			}
			if !strings.Contains(logs.String(), "skipping entry with bad path") {
				t.Errorf("memberPath(%q) refusal not logged: %s", tc.name, logs.String())
			}
			continue
		}
		if !ok || sp.Rel() != tc.wantRel {
			t.Errorf("memberPath(%q, oneFolder=%v) = %q, %v; want %q", tc.name, tc.oneFolder, sp.Rel(), ok, tc.wantRel)
		}
		if len(lines) != 0 {
			t.Errorf("memberPath(%q) reported %v for an accepted name", tc.name, lines)
		}
	}
}
