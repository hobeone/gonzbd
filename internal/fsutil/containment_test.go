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

func TestCheckContainment_NonExistentDir(t *testing.T) {
	err := CheckContainment("/non/existent/path/for/sure")
	if err == nil {
		t.Fatal("expected error for non-existent directory, got nil")
	}
	if !strings.Contains(err.Error(), "eval symlinks on dir") {
		t.Errorf("expected error to mention 'eval symlinks on dir', got: %v", err)
	}
}

func TestCheckContainment_BrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	// Create a symlink pointing to a non-existent target.
	link := filepath.Join(dir, "broken.txt")
	if err := os.Symlink("non_existent_target.txt", link); err != nil {
		t.Fatal(err)
	}

	err := CheckContainment(dir)
	if err == nil {
		t.Fatal("expected error for broken symlink, got nil")
	}
	if !strings.Contains(err.Error(), "eval symlinks") {
		t.Errorf("expected error to mention 'eval symlinks', got: %v", err)
	}
}

func TestCheckContainment_WalkErr(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "unreadable_subdir")
	if err := os.Mkdir(sub, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sub, 0755) // Clean up permission so cleanup can delete it

	err := CheckContainment(dir)
	// We expect an error. It might be permission denied (which is walked as walkErr).
	if err == nil {
		if os.Getuid() == 0 {
			t.Skip("Running as root, permission checks ignored")
		}
		t.Fatal("expected error for unreadable subdirectory, got nil")
	}
}

func TestPathWithin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		base   string
		target string
		want   bool
	}{
		// Equal
		{"/a/b", "/a/b", true},
		{"/a/b/", "/a/b", true},
		{"/a/b", "/a/b/", true},
		// Nested
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/b/c/d", true},
		// Escape
		{"/a/b", "/a", false},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a/b_suffix", false},
		{"/a/b", "/a/b/../c", false},    // resolves to /a/c, which escapes /a/b
		{"/a/b", "/a/b/../../c", false}, // resolves to /c, which escapes
		// Relative paths
		{"a/b", "a/b/c", true},
		{"a/b", "a/c", false},
		{"a/b", "a/b/../c", false},
		// Mixed absolute/relative
		{"/a/b", "/a/b/c/..", true},
	}

	for _, tt := range tests {
		t.Run(tt.base+" ↔ "+tt.target, func(t *testing.T) {
			got := PathWithin(tt.base, tt.target)
			if got != tt.want {
				t.Errorf("PathWithin(%q, %q) = %t, want %t", tt.base, tt.target, got, tt.want)
			}
		})
	}
}

func TestResolveAndVerifyContainment_SymlinkEscapes(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "evil.sh")
	if err := os.WriteFile(outsideFile, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a symlink inside baseDir that points outside.
	link := filepath.Join(baseDir, "hook.sh")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatal(err)
	}

	// Lexical PathWithin passes (the symlink path is within baseDir).
	if !PathWithin(baseDir, link) {
		t.Fatal("PathWithin should pass for the lexical path")
	}

	// ResolveAndVerifyContainment should catch the escape.
	_, err := ResolveAndVerifyContainment(baseDir, link)
	if err == nil {
		t.Fatal("ResolveAndVerifyContainment should reject symlink escaping baseDir")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("expected 'escapes' in error, got: %v", err)
	}
}

func TestResolveAndVerifyContainment_SymlinkWithin(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	target := filepath.Join(baseDir, "real.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(baseDir, "hook.sh")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	resolved, err := ResolveAndVerifyContainment(baseDir, link)
	if err != nil {
		t.Fatalf("unexpected error for symlink within base: %v", err)
	}
	// resolved should be the canonical path to real.sh
	if !PathWithin(baseDir, resolved) {
		t.Errorf("resolved path %q not within base %q", resolved, baseDir)
	}
}

func TestResolveAndVerifyContainment_NoSymlink(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	target := filepath.Join(baseDir, "script.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	resolved, err := ResolveAndVerifyContainment(baseDir, target)
	if err != nil {
		t.Fatalf("unexpected error for regular file: %v", err)
	}
	if !PathWithin(baseDir, resolved) {
		t.Errorf("resolved path %q not within base %q", resolved, baseDir)
	}
}

func TestResolveAndVerifyContainment_BrokenSymlink(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	link := filepath.Join(baseDir, "broken.sh")
	if err := os.Symlink("/nonexistent/target", link); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveAndVerifyContainment(baseDir, link)
	if err == nil {
		t.Fatal("expected error for broken symlink, got nil")
	}
}
