package fsutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// TestRootedOpenFile_Normal verifies that RootedOpenFile creates parent dirs
// and writes a file inside the root directory correctly.
func TestRootedOpenFile_Normal(t *testing.T) {
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	f, err := fsutil.RootedOpenFile(root, "subdir/file.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("RootedOpenFile: %v", err)
	}
	if _, err := f.WriteString("hello"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(outDir, "subdir", "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
}

// TestRootedOpenFile_ZipSlip_DotDot verifies that a path containing ".."
// components is refused by os.Root even when passed directly.
func TestRootedOpenFile_ZipSlip_DotDot(t *testing.T) {
	outDir := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "escape.txt")

	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	// Attempt to write outside via a dotdot path. os.Root must refuse.
	rel := "../" + filepath.Base(outside) + "/escape.txt"
	f, err := fsutil.RootedOpenFile(root, rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err == nil {
		_ = f.Close()
		t.Fatal("expected error: os.Root should refuse dotdot path")
	}

	// Victim must not have been created.
	if _, statErr := os.Stat(victim); !os.IsNotExist(statErr) {
		t.Fatal("SECURITY: file was created outside root via dotdot path")
	}
}

// TestRootedOpenFile_ZipSlip_Symlink verifies that a symlinked path component
// inside the root that points outside is refused by os.Root. This is the
// key case that lexical sanitization alone cannot catch.
func TestRootedOpenFile_ZipSlip_Symlink(t *testing.T) {
	outDir := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "evil.txt")

	// Plant a symlink inside outDir that points to outside.
	if err := os.Symlink(outside, filepath.Join(outDir, "link")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	// "link/evil.txt" is lexically inside root but resolves outside.
	f, err := fsutil.RootedOpenFile(root, "link/evil.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err == nil {
		_ = f.Close()
		t.Fatal("expected error: os.Root should refuse traversal through symlinked component")
	}

	// The victim file must not have been created outside the root.
	if _, statErr := os.Stat(victim); !os.IsNotExist(statErr) {
		t.Fatal("SECURITY: file was created outside root by traversing symlinked component")
	}
}
