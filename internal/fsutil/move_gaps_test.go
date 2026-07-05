package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ---------- MoveFile ----------

func TestMoveFile_SameDeviceRename(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("hello"), 0644)

	if err := MoveFile(src, dst); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should be removed after move")
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("content = %q, want hello", data)
	}
}

func TestMoveFile_NonexistentSrc(t *testing.T) {
	dir := t.TempDir()
	err := MoveFile(filepath.Join(dir, "nonexistent"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Error("expected error for nonexistent source")
	}
}

// ---------- copyAndRemove ----------

func TestCopyAndRemove_Regular(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.dat")
	dst := filepath.Join(dir, "dst.dat")
	os.WriteFile(src, []byte("data"), 0755)

	if err := copyAndRemove(src, dst); err != nil {
		t.Fatalf("copyAndRemove: %v", err)
	}

	data, _ := os.ReadFile(dst)
	if string(data) != "data" {
		t.Errorf("content = %q", data)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should be removed")
	}

	// Verify permissions preserved (match source, not hardcoded).
	info, _ := os.Stat(dst)
	if info.Mode().Perm()&0700 != 0700 {
		t.Errorf("mode = %v, want at least owner rwx", info.Mode().Perm())
	}
}

func TestCopyAndRemove_Symlink(t *testing.T) {
	dir := t.TempDir()

	target := filepath.Join(dir, "target.txt")
	os.WriteFile(target, []byte("target"), 0644)

	src := filepath.Join(dir, "link")
	dst := filepath.Join(dir, "link-moved")
	os.Symlink(target, src)

	if err := copyAndRemove(src, dst); err != nil {
		t.Fatalf("copyAndRemove symlink: %v", err)
	}

	got, err := os.Readlink(dst)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if got != target {
		t.Errorf("link target = %q, want %q", got, target)
	}

	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Error("source symlink should be removed")
	}
}

func TestCopyAndRemove_NonexistentSrc(t *testing.T) {
	dir := t.TempDir()
	err := copyAndRemove(filepath.Join(dir, "nope"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Error("expected error for nonexistent source")
	}
}

// ---------- IsCrossDeviceError / IsRenameMergeNeeded ----------

func TestIsCrossDeviceError_Nil(t *testing.T) {
	t.Parallel()
	if IsCrossDeviceError(nil) {
		t.Error("nil should not be cross-device error")
	}
}

func TestIsRenameMergeNeeded_Nil(t *testing.T) {
	t.Parallel()
	if IsRenameMergeNeeded(nil) {
		t.Error("nil should not need merge")
	}
}

func TestCrossDeviceErr_Direct(t *testing.T) {
	err := crossDeviceErr()
	if err == nil {
		t.Fatal("expected non-nil cross-device error")
	}
	if !IsCrossDeviceError(err) {
		t.Errorf("expected IsCrossDeviceError(%v) to be true", err)
	}
}

func TestCopyAndRemove_SymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	sym := filepath.Join(dir, "escape.txt")
	if err := os.Symlink("/tmp/nonexistent_escape_target", sym); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	dst := filepath.Join(dir, "escape_moved.txt")

	err := copyAndRemove(sym, dst)
	if err == nil {
		t.Fatal("expected ErrSymlinkEscape, got nil")
	}
	if !errors.Is(err, ErrSymlinkEscape) {
		t.Errorf("expected ErrSymlinkEscape, got: %v", err)
	}
}

func TestCopyAndRemove_CreateError(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src_create_err.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Passing a directory path as dst causes os.Create(dst) to fail, executing _ = in.Close().
	err := copyAndRemove(src, dir)
	if err == nil {
		t.Fatal("expected create error when dst is a directory, got nil")
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("src should still exist after create error: %v", err)
	}
}

func TestCopyAndRemove_CopyError(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src_dir")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	dst := filepath.Join(dir, "dst.txt")
	// Passing a directory as src allows os.Open and os.Create to succeed, but io.Copy fails with EISDIR.
	err := copyAndRemove(srcDir, dst)
	if err == nil {
		t.Fatal("expected copy error when src is a directory, got nil")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst partial file should be removed after copy error: %v", err)
	}
}

func TestCopyAndRemove_SymlinkCreateError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sym := filepath.Join(dir, "sym.txt")
	if err := os.Symlink("target.txt", sym); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	// Destination in non-existent directory causes os.Symlink to fail.
	dst := filepath.Join(dir, "nodir", "sym_dst.txt")

	err := copyAndRemove(sym, dst)
	if err == nil {
		t.Fatal("expected symlink create error when dst dir does not exist, got nil")
	}
}

func TestCopyAndRemove_OpenError(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "unreadable.txt")
	if err := os.WriteFile(src, []byte("data"), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(src, 0o600)
	})

	dst := filepath.Join(dir, "dst.txt")
	err := copyAndRemove(src, dst)
	if err == nil {
		t.Fatal("expected open error on 0000 file, got nil")
	}
}
