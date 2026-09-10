package unpack

import (
	"os"
	"path/filepath"
	"testing"
)

// openRootFor opens tmpDir as an os.Root for the uniquePath tests, which work
// in root-relative names rather than absolute paths.
func openRootFor(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func TestUniquePath(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	root := openRootFor(t, tmpDir)

	t.Run("destination path does not exist", func(t *testing.T) {
		got := uniquePath(root, "does_not_exist.txt")
		if got != "does_not_exist.txt" {
			t.Errorf("uniquePath(%q) = %q, want it unchanged", "does_not_exist.txt", got)
		}
	})

	t.Run("destination file exists once", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(tmpDir, "exists.txt"), []byte("original"), 0o644); err != nil {
			t.Fatal(err)
		}

		if got := uniquePath(root, "exists.txt"); got != "exists_1.txt" {
			t.Errorf("uniquePath(%q) = %q, want %q", "exists.txt", got, "exists_1.txt")
		}
	})

	t.Run("destination file and suffix exists", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(tmpDir, "multi.txt"), []byte("0"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "multi_1.txt"), []byte("1"), 0o644); err != nil {
			t.Fatal(err)
		}

		if got := uniquePath(root, "multi.txt"); got != "multi_2.txt" {
			t.Errorf("uniquePath(%q) = %q, want %q", "multi.txt", got, "multi_2.txt")
		}
	})

	t.Run("no extension file", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(tmpDir, "noext"), []byte("noext"), 0o644); err != nil {
			t.Fatal(err)
		}

		if got := uniquePath(root, "noext"); got != "noext_1" {
			t.Errorf("uniquePath(%q) = %q, want %q", "noext", got, "noext_1")
		}
	})

	t.Run("a name in a subdirectory keeps its directory", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(tmpDir, "sub"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "sub", "a.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		want := filepath.Join("sub", "a_1.txt")
		if got := uniquePath(root, filepath.Join("sub", "a.txt")); got != want {
			t.Errorf("uniquePath(sub/a.txt) = %q, want %q — the suffix goes on the base, not the path", got, want)
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
	root := openRootFor(t, tmpDir)

	if err := os.Symlink("no-such-target", filepath.Join(tmpDir, "file.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got := uniquePath(root, "file.txt"); got != "file_1.txt" {
		t.Errorf("uniquePath(file.txt) = %q, want the suffixed name — a dangling symlink still occupies the name", got)
	}

	// The candidate loop has its own existence test, and the check above
	// never reaches it. Occupying the first candidate with a second dangling
	// link is what forces the loop's own probe to be the one under test.
	if err := os.Symlink("no-such-target", filepath.Join(tmpDir, "file_1.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := uniquePath(root, "file.txt"); got != "file_2.txt" {
		t.Errorf("uniquePath(file.txt) = %q, want file_2.txt — a dangling symlink occupies the first candidate too", got)
	}
}

// TestNameIsFree_OnlyErrNotExistMeansFree pins that a name whose status cannot
// be determined is treated as occupied rather than available.
//
// The failure this prevents is the inverse of the dangling-symlink one: there,
// an occupied name read as free because the probe asked the wrong question;
// here, a name that cannot be probed at all would read as free because any
// error was taken for absence. Both end with a write to a name something else
// holds.
func TestNameIsFree_OnlyErrNotExistMeansFree(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	root := openRootFor(t, tmpDir)

	if !nameIsFree(root, "absent.txt") {
		t.Error("a name nothing holds was reported occupied")
	}

	if err := os.WriteFile(filepath.Join(tmpDir, "present.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if nameIsFree(root, "present.txt") {
		t.Error("a name held by a regular file was reported free")
	}

	// An escaping name is refused by os.Root with an error that is NOT
	// ErrNotExist. Reporting it free would hand the caller a name the write
	// can never reach, and the write would fail after the entry was already
	// counted as placed.
	if nameIsFree(root, filepath.Join("..", "escape.txt")) {
		t.Error("a name os.Root refuses was reported free; the error is an escape, not an absence")
	}
}
