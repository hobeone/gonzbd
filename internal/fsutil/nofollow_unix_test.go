//go:build unix

package fsutil_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

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
	t.Run("a hard link is refused and its sibling is untouched", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "linked.bin"), []byte("LINKED"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(dir, "linked.bin"), filepath.Join(dir, "hard.bin")); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"hard.bin", "linked.bin"} {
			fh, err := fsutil.OpenNoFollow(root, name, os.O_RDWR|os.O_CREATE, 0o600)
			if !errors.Is(err, fsutil.ErrMultiplyLinked) {
				t.Errorf("%s: err = %v, want ErrMultiplyLinked", name, err)
			}
			if fh != nil {
				_ = fh.Close()
			}
		}
		if got, err := os.ReadFile(filepath.Join(dir, "linked.bin")); string(got) != "LINKED" {
			t.Errorf("linked.bin = %q, %v; want it untouched", got, err)
		}
	})
	t.Run("O_TRUNC is refused before the open", func(t *testing.T) {
		if _, err := fsutil.OpenNoFollow(root, "sibling.bin", os.O_WRONLY|os.O_TRUNC, 0); err == nil {
			t.Error("an O_TRUNC open succeeded; it truncates before the link check")
		}
		if got, err := os.ReadFile(filepath.Join(dir, "sibling.bin")); string(got) != "SIB" {
			t.Errorf("sibling.bin = %q, %v; want it untouched", got, err)
		}
	})
	t.Run("a FIFO is refused without blocking", func(t *testing.T) {
		if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, flag := range []int{os.O_RDONLY, os.O_RDWR} {
			if _, err := fsutil.OpenNoFollow(root, "fifo", flag, 0); !errors.Is(err, fsutil.ErrNotRegular) {
				t.Errorf("flag %#x: err = %v, want ErrNotRegular", flag, err)
			}
		}
	})
	t.Run("a returned descriptor is blocking", func(t *testing.T) {
		fh, err := fsutil.OpenNoFollow(root, "sibling.bin", os.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = fh.Close() }()
		fl, err := unix.FcntlInt(fh.Fd(), unix.F_GETFL, 0)
		if err != nil {
			t.Fatal(err)
		}
		if fl&unix.O_NONBLOCK != 0 {
			t.Errorf("descriptor flags %#x carry O_NONBLOCK", fl)
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
