package unpack

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUniquePath(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	t.Run("destination path does not exist", func(t *testing.T) {
		dest := filepath.Join(tmpDir, "does_not_exist.txt")
		got := uniquePath(dest)
		if got != dest {
			t.Errorf("uniquePath(%q) = %q, want %q", dest, got, dest)
		}
	})

	t.Run("destination file exists once", func(t *testing.T) {
		dest := filepath.Join(tmpDir, "exists.txt")
		if err := os.WriteFile(dest, []byte("original"), 0o644); err != nil {
			t.Fatal(err)
		}

		want := filepath.Join(tmpDir, "exists_1.txt")
		got := uniquePath(dest)
		if got != want {
			t.Errorf("uniquePath(%q) = %q, want %q", dest, got, want)
		}
	})

	t.Run("destination file and suffix exists", func(t *testing.T) {
		dest := filepath.Join(tmpDir, "multi.txt")
		if err := os.WriteFile(dest, []byte("0"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "multi_1.txt"), []byte("1"), 0o644); err != nil {
			t.Fatal(err)
		}

		want := filepath.Join(tmpDir, "multi_2.txt")
		got := uniquePath(dest)
		if got != want {
			t.Errorf("uniquePath(%q) = %q, want %q", dest, got, want)
		}
	})

	t.Run("no extension file", func(t *testing.T) {
		dest := filepath.Join(tmpDir, "noext")
		if err := os.WriteFile(dest, []byte("noext"), 0o644); err != nil {
			t.Fatal(err)
		}

		want := filepath.Join(tmpDir, "noext_1")
		got := uniquePath(dest)
		if got != want {
			t.Errorf("uniquePath(%q) = %q, want %q", dest, got, want)
		}
	})

	t.Run("empty dest path", func(t *testing.T) {
		got := uniquePath("")
		if got != "" {
			t.Errorf("uniquePath(\"\") = %q, want \"\"", got)
		}
	})
}
