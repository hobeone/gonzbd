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

// The cross-device fallback refuses a symlink rather than recreating it, even
// one whose target stays beside it: a job holds no links
// (docs/post-processing-contract.md § "No links in a job"). The link is left
// where it was and nothing is created at dst.
func TestCopyAndRemove_RefusesSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "link")
	if err := os.Symlink("target.txt", src); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "link")

	err := copyAndRemove(src, dst)
	if !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("copyAndRemove(symlink) = %v, want ErrNotRegularFile", err)
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Errorf("dst created for a refused symlink: Lstat err = %v", err)
	}
	if fi, err := os.Lstat(src); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("source symlink not left in place: %v, %v", fi, err)
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

// A read error mid-copy removes the partial dst. /proc/self/mem is a regular
// file whose read at offset 0 fails with EIO, so it reaches io.Copy past the
// regular-file check; its own Remove is never attempted on this path.
func TestCopyAndRemove_CopyErrorRemovesPartialDst(t *testing.T) {
	t.Parallel()
	const src = "/proc/self/mem"
	if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() {
		t.Skipf("%s is not a regular file here: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	err := copyAndRemove(src, dst)
	if err == nil || errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("copyAndRemove(%s) = %v, want a read error", src, err)
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Errorf("partial dst left after copy error: Lstat err = %v", err)
	}
}

// A directory is not a regular file either: the fallback refuses it before
// opening anything, rather than creating dst and failing in io.Copy.
func TestCopyAndRemove_RefusesDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src_dir")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	dst := filepath.Join(dir, "dst.txt")
	if err := copyAndRemove(srcDir, dst); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("copyAndRemove(dir) = %v, want ErrNotRegularFile", err)
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Errorf("dst created for a refused directory: Lstat err = %v", err)
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
