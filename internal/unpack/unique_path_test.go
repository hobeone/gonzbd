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

// TestUniquePath_DanglingSymlinkOccupiesTheName pins that a name held by a
// symlink to nothing is treated as taken.
//
// Stat follows the link and fails on the missing target, which reads as "the
// name is free" — so the caller writes to the undecorated name and the write
// follows the link to wherever it points. Lstat answers about the link itself,
// which is the entry actually occupying the name.
func TestUniquePath_DanglingSymlinkOccupiesTheName(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	dest := filepath.Join(tmpDir, "file.txt")
	if err := os.Symlink("no-such-target", dest); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got := uniquePath(dest); got != filepath.Join(tmpDir, "file_1.txt") {
		t.Errorf("uniquePath(%q) = %q, want the suffixed name — a dangling symlink still occupies the name", dest, got)
	}

	// The candidate loop has its own existence test, and the check above
	// never reaches it. Occupying the first candidate with a second dangling
	// link is what forces the loop's own Lstat to be the one under test.
	if err := os.Symlink("no-such-target", filepath.Join(tmpDir, "file_1.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := uniquePath(dest); got != filepath.Join(tmpDir, "file_2.txt") {
		t.Errorf("uniquePath(%q) = %q, want file_2.txt — a dangling symlink occupies the first candidate too", dest, got)
	}
}
