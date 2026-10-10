//go:build unix

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrNotOneComponent is returned by OpenNoFollow for a name that is not a
// single path component.
var ErrNotOneComponent = errors.New("name is not a single path component")

// OpenNoFollow opens name inside root with flag and perm, refusing a symlink
// even when its target stays inside root. os.Root follows such a link, and
// ignores O_NOFOLLOW for it.
//
// name must be one path component: not empty, not "." or "..", and without a
// separator; anything else is an *os.PathError wrapping ErrNotOneComponent.
// The open is one openat(2) on root's directory with O_NOFOLLOW, so a symlink
// in name's place, dangling or not, fails with ELOOP and nothing is created or
// opened through it. There is no window between a check and the open. Other
// errors are openat's own, wrapped in an *os.PathError, so a missing file is
// still fs.ErrNotExist.
func OpenNoFollow(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return nil, &os.PathError{Op: "open", Path: name, Err: ErrNotOneComponent}
	}
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }() // a directory handle; nothing to lose on close
	fd, err := unix.Openat(int(dir.Fd()), name, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: filepath.Join(root.Name(), name), Err: err}
	}
	return os.NewFile(uintptr(fd), filepath.Join(root.Name(), name)), nil
}
