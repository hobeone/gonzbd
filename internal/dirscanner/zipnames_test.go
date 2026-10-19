package dirscanner

import (
	"archive/zip"
	"os"
	"path/filepath"
	"slices"
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

// TestMemberBaseName_NeverCarriesADirectory pins that an untrusted member name
// is reduced to one path element.
func TestMemberBaseName_NeverCarriesADirectory(t *testing.T) {
	for in, want := range map[string]string{
		"a.nzb":           "a.nzb",
		"d/a.nzb":         "a.nzb",
		"../../etc/a.nzb": "a.nzb",
		"/abs/a.nzb":      "a.nzb",
		`C:\x\a.nzb`:      "a.nzb",
		`..\..\a.nzb`:     "a.nzb",
		"..":              "",
		"dir/..":          "",
		"":                "",
	} {
		if got := memberBaseName(in); got != want {
			t.Errorf("memberBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}
