//go:build unix

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrNotOneComponent is returned by OpenNoFollow for a name that is not a
// single path component.
var ErrNotOneComponent = errors.New("name is not a single path component")

// ErrMultiplyLinked is returned by OpenNoFollow for a file with more than one
// hard link.
var ErrMultiplyLinked = errors.New("file has more than one hard link")

// ErrNotRegular is returned by OpenNoFollow for a name that is not a regular
// file.
var ErrNotRegular = errors.New("not a regular file")

var errNoTrunc = errors.New("O_TRUNC is not supported: it would truncate before the link check")

// OpenNoFollow opens name inside root with flag and perm, and returns it only
// when it is a regular file with exactly one link: the file is reachable by
// this name and no other, so writing or truncating it changes no other file.
//
// name must be one path component: not empty, not "." or "..", and without a
// separator; anything else is an *os.PathError wrapping ErrNotOneComponent.
// flag must not carry O_TRUNC, which the kernel applies at the open, before
// the link check below could refuse the file.
// The open is one openat(2) on root's directory with O_NOFOLLOW, so a symlink
// in name's place fails with ELOOP wherever it points, even inside root, which
// os.Root alone would follow (it ignores O_NOFOLLOW for such a link). Nothing
// is created or opened through a symlink, and there is no window between a
// check and the open. The open is non-blocking, so a FIFO in name's place cannot
// hang it; the descriptor is made blocking again before it is returned.
//
// The opened inode is then checked with fstat(2): a hard link is a regular
// file that O_NOFOLLOW cannot see, so a file with more than one link is
// refused with ErrMultiplyLinked, and anything but a regular file with
// ErrNotRegular, each in an *os.PathError and with the descriptor closed. The
// check is on the descriptor itself, so it too has no window. A file this
// function just created has one link.
//
// Its callers open a download's own files — `git grep -n '[O]penNoFollow(' -- '*.go' ':!*_test.go'`
// finds 4 lines: this declaration, the assembler's openInDir, and the
// verifier's readBackFile and finishFileByPath. A job file is always a
// regular file with one link, which the writer created, so a refusal here is
// never a legitimate file turned away.
//
// Other errors are openat's own, wrapped in an *os.PathError, so a missing
// file is still fs.ErrNotExist.
func OpenNoFollow(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return nil, &os.PathError{Op: "open", Path: name, Err: ErrNotOneComponent}
	}
	if flag&os.O_TRUNC != 0 {
		// O_TRUNC would act at the open, before the inode check below.
		return nil, &os.PathError{Op: "open", Path: name, Err: errNoTrunc}
	}
	path := filepath.Join(root.Name(), name)
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }() // a directory handle; nothing to lose on close
	var fd int
	for {
		// EINTR: an open on a network or FUSE mount can be interrupted by the
		// runtime's preemption signal; os retries its own opens the same way.
		fd, err = unix.Openat(int(dir.Fd()), name,
			flag|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, uint32(perm.Perm()))
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: path, Err: err}
	}
	if err := checkSoleRegular(fd); err != nil {
		_ = unix.Close(fd) // refused before any use; nothing to lose on close
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd) // unused descriptor; nothing to lose on close
		return nil, &os.PathError{Op: "fcntl", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// checkSoleRegular reports whether fd is a regular file with one link.
func checkSoleRegular(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("fstat: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrNotRegular
	}
	if st.Nlink != 1 {
		return ErrMultiplyLinked
	}
	return nil
}
