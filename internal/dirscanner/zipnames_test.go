package dirscanner

import (
	"archive/zip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeNamedZip(t *testing.T, path string, names []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test cleanup
	w := zip.NewWriter(f)
	for _, n := range names {
		fw, err := w.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte("<nzb/>")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestHandleStableFile_ZipMembersNamedAfterMembers pins that each NZB in a zip
// reaches the handler under its own member name, and that a plain .nzb still
// reaches it under the file's name.
func TestHandleStableFile_ZipMembersNamedAfterMembers(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "bundle.zip")
	writeNamedZip(t, zipPath, []string{"Show.S01E01.nzb", "readme.txt", "sub/Show.S01E02.nzb"})
	plain := filepath.Join(dir, "plain.nzb")
	if err := os.WriteFile(plain, []byte("<nzb/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &MockHandler{failFor: map[string]error{}}
	s := New(dir, NewStore(), h, nil, nil)
	if err := s.handleStableFile(t.Context(), zipPath, "bundle.zip", ""); err != nil {
		t.Fatalf("zip: %v", err)
	}
	if err := s.handleStableFile(t.Context(), plain, "plain.nzb", ""); err != nil {
		t.Fatalf("plain: %v", err)
	}

	got := make([]string, 0, len(h.calls))
	for _, c := range h.calls {
		got = append(got, c.Filename)
	}
	want := []string{"Show.S01E01.nzb", "Show.S01E02.nzb", "plain.nzb"}
	if !slices.Equal(got, want) {
		t.Errorf("handler names = %q, want %q", got, want)
	}
}

// TestHandleStableFile_ZipMemberNamesAreDistinct pins that no two NZBs of one
// archive reach the handler under the same filename, whether the names repeat
// across subdirectories, are rejected, or are not valid UTF-8.
func TestHandleStableFile_ZipMemberNamesAreDistinct(t *testing.T) {
	cases := map[string]struct {
		members []string
		want    []string
	}{
		"same base name in two dirs": {
			[]string{"a/x.nzb", "b/x.nzb"}, []string{"x.nzb", "z.zip[2]"},
		},
		"two rejected names": {
			[]string{"a\x00.nzb", "b\x00.nzb"}, []string{"z.zip[1]", "z.zip[2]"},
		},
		"invalid UTF-8 name": {
			[]string{"ok.nzb", "caf\xe9.nzb"}, []string{"ok.nzb", "z.zip[2]"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			zipPath := filepath.Join(dir, "z.zip")
			writeNamedZip(t, zipPath, tc.members)
			h := &MockHandler{failFor: map[string]error{}}
			s := New(dir, NewStore(), h, nil, nil)
			if err := s.handleStableFile(t.Context(), zipPath, "z.zip", ""); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(h.calls))
			for _, c := range h.calls {
				got = append(got, c.Filename)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("handler names = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMemberBaseName_NeverCarriesADirectory pins that an untrusted member name
// is reduced to one path element.
func TestMemberBaseName_NeverCarriesADirectory(t *testing.T) {
	for in, want := range map[string]string{
		"a.nzb":                           "a.nzb",
		"d/a.nzb":                         "a.nzb",
		"../../etc/a.nzb":                 "a.nzb",
		"/abs/a.nzb":                      "a.nzb",
		`C:\x\a.nzb`:                      "a.nzb",
		`..\..\a.nzb`:                     "a.nzb",
		"..":                              "",
		"dir/..":                          "",
		"":                                "",
		"a\x00b.nzb":                      "",
		"a\tb.nzb":                        "",
		"d/.nzb":                          "",
		"caf\xe9.nzb":                     "",
		strings.Repeat("x", 300) + ".nzb": "",
		strings.Repeat("x", 195) + ".nzb": strings.Repeat("x", 195) + ".nzb",
	} {
		if got := memberBaseName(in); got != want {
			t.Errorf("memberBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}
