//go:build unix

package fsutil_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

func TestOpenNoFollow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sibling.bin"), []byte("SIB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sibling.bin", filepath.Join(dir, "link.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing.bin"), filepath.Join(dir, "dangling.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	t.Run("a regular file opens and a new one is created", func(t *testing.T) {
		for _, name := range []string{"sibling.bin", "new.bin"} {
			fh, err := fsutil.OpenNoFollow(root, name, os.O_WRONLY|os.O_CREATE, 0o600)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if want := filepath.Join(dir, name); fh.Name() != want {
				t.Errorf("Name() = %q, want %q", fh.Name(), want)
			}
			_ = fh.Close()
		}
	})
	t.Run("a symlink is refused wherever it points", func(t *testing.T) {
		for _, name := range []string{"link.bin", "dangling.bin"} {
			fh, err := fsutil.OpenNoFollow(root, name, os.O_WRONLY|os.O_CREATE, 0o600)
			if !errors.Is(err, syscall.ELOOP) {
				t.Errorf("%s: err = %v, want ELOOP", name, err)
			}
			if fh != nil {
				_ = fh.Close()
			}
		}
		if _, err := os.Lstat(filepath.Join(dir, "missing.bin")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the dangling link's target was created: %v", err)
		}
	})
	t.Run("a missing file is ErrNotExist", func(t *testing.T) {
		if _, err := fsutil.OpenNoFollow(root, "absent.bin", os.O_RDONLY, 0); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want ErrNotExist", err)
		}
	})
	t.Run("a name that is not one component is refused", func(t *testing.T) {
		for _, name := range []string{"", ".", "..", "sub/x.bin", "../x.bin"} {
			if _, err := fsutil.OpenNoFollow(root, name, os.O_WRONLY|os.O_CREATE, 0o600); !errors.Is(err, fsutil.ErrNotOneComponent) {
				t.Errorf("%q: err = %v, want ErrNotOneComponent", name, err)
			}
		}
		if _, err := os.Lstat(filepath.Join(dir, "sub", "x.bin")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("sub/x.bin was created: %v", err)
		}
	})
}
