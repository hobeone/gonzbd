package unpack

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memberPath is the destination rule every in-process extractor applies to
// each member. rarengine rewrites "../" out of names before they reach it, so
// a fixture cannot exercise the name check; it is driven with the names
// directly, against a root that already holds "taken.txt".
func TestMemberPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "taken.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	for _, tc := range []struct {
		name      string
		isDir     bool
		opts      Options
		wantRel   string // "" means it creates nothing
		wantBadOK bool   // a refused name is logged and reported
	}{
		{name: "../x", wantBadOK: true},
		{name: "a/../../x", wantBadOK: true},
		{name: `..\..\etc\passwd`, wantBadOK: true},
		{name: "a/b\x00c", wantBadOK: true},
		{name: "", wantBadOK: true},
		{name: ".", wantBadOK: true},
		{name: "/etc/passwd", wantRel: "etc/passwd"},
		{name: `dir\sub\file.mkv`, wantRel: "dir/sub/file.mkv"},
		{name: "a/./b//c.txt", wantRel: "a/b/c.txt"},
		{name: "movie.mkv", wantRel: "movie.mkv"},
		{name: "taken.txt", wantRel: "taken.txt"},
		{name: "a/dir", isDir: true, wantRel: "a/dir"},
		{name: "../x", opts: Options{OneFolder: true}, wantRel: "x"},
		{name: "sub/taken.txt", opts: Options{OneFolder: true}, wantRel: "taken_1.txt"},
		{name: "sub/taken.txt", opts: Options{OneFolder: true, OverwriteFiles: true}, wantRel: "taken.txt"},
		{name: "a/dir", isDir: true, opts: Options{OneFolder: true}},
		{name: "a/dir", isDir: true, opts: Options{OneFolder: true, OverwriteFiles: true}},
	} {
		var lines []string
		var logs strings.Builder
		opts := tc.opts
		opts.OnLine = func(l string) { lines = append(lines, l) }
		rel, ok := memberPath(root, tc.name, tc.isDir, opts, slog.New(slog.NewTextHandler(&logs, nil)))
		if tc.wantBadOK {
			if ok {
				t.Errorf("memberPath(%q) accepted as %q, want refused", tc.name, rel)
			}
			if len(lines) != 1 || lines[0] != "Skipping bad path: "+tc.name {
				t.Errorf("memberPath(%q) reported %v", tc.name, lines)
			}
			if !strings.Contains(logs.String(), "skipping entry with bad path") {
				t.Errorf("memberPath(%q) refusal not logged: %s", tc.name, logs.String())
			}
			continue
		}
		if tc.wantRel == "" {
			if ok || rel != "" {
				t.Errorf("memberPath(%q, dir=%v, flat=%v, overwrite=%v) = %q, %v; want nothing created", tc.name, tc.isDir, tc.opts.OneFolder, tc.opts.OverwriteFiles, rel, ok)
			}
		} else if !ok || rel != tc.wantRel {
			t.Errorf("memberPath(%q, dir=%v, flat=%v, overwrite=%v) = %q, %v; want %q", tc.name, tc.isDir, tc.opts.OneFolder, tc.opts.OverwriteFiles, rel, ok, tc.wantRel)
		}
		if len(lines) != 0 {
			t.Errorf("memberPath(%q) reported %v for a name it did not refuse", tc.name, lines)
		}
	}
}
