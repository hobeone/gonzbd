package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCopyAndRemove_SymlinkContained verifies that a symlink pointing to a
// file within the source directory is recreated successfully at the destination.
func TestCopyAndRemove_SymlinkContained(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create a target file inside the source directory.
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create a relative symlink to the sibling file.
	src := filepath.Join(dir, "link")
	if err := os.Symlink("real.txt", src); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	dst := filepath.Join(dir, "link-moved")
	if err := copyAndRemove(src, dst); err != nil {
		t.Fatalf("copyAndRemove contained symlink: %v", err)
	}

	// Destination should be a symlink with the same relative target.
	got, err := os.Readlink(dst)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if got != "real.txt" {
		t.Errorf("symlink target = %q, want %q", got, "real.txt")
	}

	// Source should be removed.
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Error("source symlink should be removed")
	}
}

// TestCopyAndRemove_SymlinkEscapesRelative verifies that a relative symlink
// pointing outside the source directory is rejected.
func TestCopyAndRemove_SymlinkEscapesRelative(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create an outside target.
	outerDir := t.TempDir()
	outerFile := filepath.Join(outerDir, "secret.txt")
	if err := os.WriteFile(outerFile, []byte("secret"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create a relative symlink that escapes via ../
	rel, err := filepath.Rel(dir, outerFile)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	src := filepath.Join(dir, "escape-link")
	if err := os.Symlink(rel, src); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	dst := filepath.Join(dir, "escape-link-moved")
	err = copyAndRemove(src, dst)
	if err == nil {
		t.Fatal("expected error for escaping symlink, got nil")
	}
	if !errors.Is(err, ErrSymlinkEscape) {
		t.Errorf("error = %v, want ErrSymlinkEscape", err)
	}

	// Destination should NOT have been created.
	if _, err := os.Lstat(dst); err == nil {
		t.Error("destination should not exist for escaping symlink")
	}
}

// TestCopyAndRemove_SymlinkEscapesAbsolute verifies that an absolute symlink
// pointing outside the source directory is rejected.
func TestCopyAndRemove_SymlinkEscapesAbsolute(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	src := filepath.Join(dir, "abs-link")
	if err := os.Symlink("/etc/passwd", src); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	dst := filepath.Join(dir, "abs-link-moved")
	err := copyAndRemove(src, dst)
	if err == nil {
		t.Fatal("expected error for absolute escaping symlink, got nil")
	}
	if !errors.Is(err, ErrSymlinkEscape) {
		t.Errorf("error = %v, want ErrSymlinkEscape", err)
	}
}

// TestCheckSymlinkContainment_SelfTarget verifies that a symlink pointing
// to its own directory passes containment (edge case).
func TestCheckSymlinkContainment_SelfTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	symlinkPath := filepath.Join(dir, "self-link")

	// Pointing to the directory itself (".")
	if err := checkSymlinkContainment(symlinkPath, "."); err != nil {
		t.Errorf("self-target should pass containment: %v", err)
	}
}

// TestCheckSymlinkContainment_SubdirTarget verifies that symlinks to
// subdirectories pass containment.
func TestCheckSymlinkContainment_SubdirTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	symlinkPath := filepath.Join(dir, "sub-link")
	if err := checkSymlinkContainment(symlinkPath, "subdir/file.txt"); err != nil {
		t.Errorf("subdir target should pass containment: %v", err)
	}
}

// A "../lib/x" link between sibling directories of one job is valid: it stays
// inside the job root. Checking it against the link's own directory refused it
// and failed the cross-device finalize.
func TestCopyAndRemoveWithin_SiblingDirectoryLinkStaysInsideRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, d := range []string{"lib", "sub"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "sub", "link")
	if err := os.Symlink("../lib/x", src); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "link")

	if err := copyAndRemoveWithin(root, src, dst); err != nil {
		t.Fatalf("copyAndRemoveWithin: %v", err)
	}
	if got, err := os.Readlink(dst); err != nil || got != "../lib/x" {
		t.Fatalf("moved link -> %q, %v", got, err)
	}
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Errorf("source link still present: %v", err)
	}
}

func TestCopyAndRemoveWithin_LinkLeavingRootStillRefused(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "job")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "sub", "link")
	if err := os.Symlink("../../outside", src); err != nil {
		t.Fatal(err)
	}
	err := copyAndRemoveWithin(root, src, filepath.Join(t.TempDir(), "link"))
	if !errors.Is(err, ErrSymlinkEscape) {
		t.Fatalf("err = %v, want ErrSymlinkEscape", err)
	}
}
