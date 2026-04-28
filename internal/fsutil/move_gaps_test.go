package fsutil

import (
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
