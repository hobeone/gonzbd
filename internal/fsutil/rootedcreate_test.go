package fsutil_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// TestRootedCreateTemp_NearNameMaxBasename verifies that a target whose
// basename is already near the filesystem's per-component length limit
// (NAME_MAX, 255 bytes on Linux) doesn't cause the temp name to overflow it.
// The temp name must not incorporate rel's own basename length.
func TestRootedCreateTemp_NearNameMaxBasename(t *testing.T) {
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	// 250 bytes: valid on Linux (NAME_MAX=255) but leaves only 5 bytes of
	// margin -- the old base+".gonzbd-tmp-<16 hex chars>" scheme added ~28
	// bytes, which would have overflowed and returned ENAMETOOLONG for a
	// perfectly valid entry name.
	longBase := strings.Repeat("a", 250)

	f, tmpRel, err := fsutil.RootedCreateTemp(t.Context(), root, longBase)
	if err != nil {
		t.Fatalf("RootedCreateTemp: %v", err)
	}
	defer func() { _ = f.Close() }()

	if strings.Contains(filepath.Base(tmpRel), longBase) {
		t.Errorf("tmpRel = %q; temp name must not incorporate rel's basename", tmpRel)
	}
}

// TestRootedCreateTemp_SameDirAsTargetAndRenamable verifies that
// RootedCreateTemp creates the temp file in the same directory as rel (so a
// subsequent rename is same-filesystem) and that the returned name is
// distinct from rel but renamable into place.
func TestRootedCreateTemp_SameDirAsTargetAndRenamable(t *testing.T) {
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	f, tmpRel, err := fsutil.RootedCreateTemp(t.Context(), root, "subdir/entry.txt")
	if err != nil {
		t.Fatalf("RootedCreateTemp: %v", err)
	}
	if tmpRel == "subdir/entry.txt" {
		t.Fatalf("tmpRel = %q; want a distinct temp name", tmpRel)
	}
	if filepath.Dir(tmpRel) != "subdir" {
		t.Errorf("tmpRel dir = %q; want %q (same dir as target)", filepath.Dir(tmpRel), "subdir")
	}
	if _, err := f.WriteString("content"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := root.Rename(tmpRel, "subdir/entry.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "subdir", "entry.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "content" {
		t.Errorf("content = %q, want %q", got, "content")
	}
}

// TestRootedCreateTemp_MkdirAllFailure verifies that a failure creating rel's
// parent directory (e.g. a regular file already occupies that path
// component) is returned as an error rather than panicking or silently
// falling through to OpenFile.
func TestRootedCreateTemp_MkdirAllFailure(t *testing.T) {
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	// "blocker" exists as a regular file, so MkdirAll("blocker", ...) for
	// rel="blocker/entry.txt" must fail: it can't create a directory where
	// a file already exists.
	if err := os.WriteFile(filepath.Join(outDir, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}

	_, _, err = fsutil.RootedCreateTemp(t.Context(), root, "blocker/entry.txt")
	if err == nil {
		t.Fatal("expected an error when the parent path is occupied by a regular file")
	}
}

// TestRootedCreateTemp_OpenFileNonExistError verifies that an OpenFile
// failure other than a name collision (fs.ErrExist) is returned immediately
// rather than retried as if it were a collision.
func TestRootedCreateTemp_OpenFileNonExistError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission checks")
	}
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	// A read-only directory rejects file creation with a permission error,
	// not fs.ErrExist -- exercising RootedCreateTemp's non-collision error
	// return path instead of the retry loop.
	if err := os.Chmod(outDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer func() {
		_ = os.Chmod(outDir, 0o755)
	}()

	_, _, err = fsutil.RootedCreateTemp(t.Context(), root, "entry.txt")
	if err == nil {
		t.Fatal("expected an error creating a temp file in a read-only directory")
	}
}

// TestRootedCreateTemp_CanceledContext verifies that a canceled context is
// honored before any filesystem work: no directory or temp file is created.
func TestRootedCreateTemp_CanceledContext(t *testing.T) {
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err = fsutil.RootedCreateTemp(ctx, root, "subdir/entry.txt")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("RootedCreateTemp error = %v, want context.Canceled", err)
	}

	entries, readErr := os.ReadDir(outDir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("outDir not empty after canceled context: %v", entries)
	}
}

// TestRootedCreateTemp_UniqueAcrossCalls verifies that repeated calls for the
// same target rel never collide with each other.
func TestRootedCreateTemp_UniqueAcrossCalls(t *testing.T) {
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	seen := make(map[string]bool)
	for range 5 {
		f, tmpRel, err := fsutil.RootedCreateTemp(t.Context(), root, "entry.txt")
		if err != nil {
			t.Fatalf("RootedCreateTemp: %v", err)
		}
		if seen[tmpRel] {
			t.Fatalf("tmpRel %q collided with a previous call", tmpRel)
		}
		seen[tmpRel] = true
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

func TestGetUniqueRelPath_AllCases(t *testing.T) {
	t.Parallel()

	t.Run("non-existent file", func(t *testing.T) {
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatalf("OpenRoot failed: %v", err)
		}
		defer root.Close()

		got := fsutil.GetUniqueRelPath(root, "newfile.txt")
		if got != "newfile.txt" {
			t.Errorf("GetUniqueRelPath = %q; want %q", got, "newfile.txt")
		}
	})

	t.Run("colliding file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "colliding.txt"), []byte("exist"), 0o644); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatalf("OpenRoot failed: %v", err)
		}
		defer root.Close()

		got := fsutil.GetUniqueRelPath(root, "colliding.txt")
		if got != "colliding.1.txt" {
			t.Errorf("GetUniqueRelPath = %q; want %q", got, "colliding.1.txt")
		}
	})

	t.Run("conflicting permission on file", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "restricted.txt")
		if err := os.WriteFile(target, []byte("restricted"), 0o000); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}
		defer func() {
			_ = os.Chmod(target, 0o644)
		}()

		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatalf("OpenRoot failed: %v", err)
		}
		defer root.Close()

		got := fsutil.GetUniqueRelPath(root, "restricted.txt")
		if got != "restricted.1.txt" {
			t.Errorf("GetUniqueRelPath = %q; want %q", got, "restricted.1.txt")
		}
	})

	t.Run("inaccessible parent directory", func(t *testing.T) {
		dir := t.TempDir()
		parent := filepath.Join(dir, "locked_dir")
		if err := os.Mkdir(parent, 0o755); err != nil {
			t.Fatalf("Mkdir failed: %v", err)
		}
		if err := os.WriteFile(filepath.Join(parent, "file.txt"), []byte("file"), 0o644); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatalf("OpenRoot failed: %v", err)
		}
		defer root.Close()

		// Lock parent directory so we cannot stat any suffix files (like locked_dir/file.1.txt).
		if err := os.Chmod(parent, 0o000); err != nil {
			t.Fatalf("Chmod failed: %v", err)
		}
		defer func() {
			_ = os.Chmod(parent, 0o755)
		}()

		// When parent directory is locked, root.Stat("locked_dir/file.1.txt") fails with permission error.
		// The loop should stop immediately and return locked_dir/file.1.txt rather than iterating 10,000 times.
		got := fsutil.GetUniqueRelPath(root, "locked_dir/file.txt")
		if got != "locked_dir/file.1.txt" {
			t.Errorf("GetUniqueRelPath = %q; want %q", got, "locked_dir/file.1.txt")
		}
	})
}
