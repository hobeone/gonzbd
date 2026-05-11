package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckContainment_AllInside(t *testing.T) {
	dir := t.TempDir()
	// Create a normal file and a subdirectory with a file.
	if err := os.WriteFile(filepath.Join(dir, "safe.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "nested.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := CheckContainment(dir); err != nil {
		t.Errorf("expected no error for contained files, got: %v", err)
	}
}

func TestCheckContainment_SymlinkInside(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := CheckContainment(dir); err != nil {
		t.Errorf("expected no error for symlink inside dir, got: %v", err)
	}
}

func TestCheckContainment_SymlinkOutside(t *testing.T) {
	dir := t.TempDir()
	// Create a file outside dir.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create a symlink inside dir pointing outside.
	link := filepath.Join(dir, "escape.txt")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatal(err)
	}

	err := CheckContainment(dir)
	if err == nil {
		t.Fatal("expected error for symlink escaping dir, got nil")
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Errorf("expected error to mention 'outside', got: %v", err)
	}
}

func TestCheckContainment_SymlinkDirOutside(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("s"), 0644); err != nil {
		t.Fatal(err)
	}
	// Symlink a directory inside dir pointing to outsideDir.
	link := filepath.Join(dir, "escaped_dir")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Fatal(err)
	}

	err := CheckContainment(dir)
	if err == nil {
		t.Fatal("expected error for directory symlink escaping dir, got nil")
	}
}

func TestCheckContainment_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := CheckContainment(dir); err != nil {
		t.Errorf("expected no error for empty dir, got: %v", err)
	}
}

func TestCheckContainment_RelativeSymlinkInside(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create a relative symlink that stays inside dir.
	link := filepath.Join(dir, "rel_link.txt")
	if err := os.Symlink("target.txt", link); err != nil {
		t.Fatal(err)
	}

	if err := CheckContainment(dir); err != nil {
		t.Errorf("expected no error for relative symlink inside dir, got: %v", err)
	}
}
