package unpack

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUniquePath_NoConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	got := uniquePath(path)
	if got != path {
		t.Errorf("uniquePath(%q) = %q, want %q (no conflict)", path, got, path)
	}
}

func TestUniquePath_SingleConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}

	got := uniquePath(path)
	want := filepath.Join(dir, "file_1.txt")
	if got != want {
		t.Errorf("uniquePath(%q) = %q, want %q", path, got, want)
	}
}

func TestUniquePath_MultipleConflicts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.csv")
	// Create data.csv, data_1.csv, data_2.csv
	for _, name := range []string{"data.csv", "data_1.csv", "data_2.csv"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	got := uniquePath(path)
	want := filepath.Join(dir, "data_3.csv")
	if got != want {
		t.Errorf("uniquePath(%q) = %q, want %q", path, got, want)
	}
}

func TestUniquePath_NoExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "README")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}

	got := uniquePath(path)
	want := filepath.Join(dir, "README_1")
	if got != want {
		t.Errorf("uniquePath(%q) = %q, want %q", path, got, want)
	}
}
