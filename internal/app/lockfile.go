//go:build unix

package app

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// ErrLocked is returned by AcquireLockfile when the file is already held
// by another process or another open file description in this process.
var ErrLocked = errors.New("app: lockfile already held")

// Lockfile is an exclusive advisory lock obtained via flock(2). The lock
// is released when Release is called or when the process exits.
type Lockfile struct {
	path string
	f    *os.File
}

// AcquireLockfile opens or creates path and takes an exclusive,
// non-blocking flock on it. Returns ErrLocked if another holder already
// has the lock. The caller's PID is written to the file for observability.
func AcquireLockfile(path string) (*Lockfile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644) //nolint:gosec // lockfile path is caller-controlled; admin dir by convention
	if err != nil {
		return nil, fmt.Errorf("open lockfile %s: %w", path, err)
	}
	if err := syscall.Flock(fdOf(f), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close() //nolint:errcheck // cleanup on error path
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(fdOf(f), syscall.LOCK_UN) //nolint:errcheck // cleanup on error path
		_ = f.Close()                               //nolint:errcheck // cleanup on error path
		return nil, fmt.Errorf("truncate lockfile %s: %w", path, err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		_ = syscall.Flock(fdOf(f), syscall.LOCK_UN) //nolint:errcheck // cleanup on error path
		_ = f.Close()                               //nolint:errcheck // cleanup on error path
		return nil, fmt.Errorf("write pid to lockfile %s: %w", path, err)
	}
	return &Lockfile{path: path, f: f}, nil
}

// Path returns the on-disk path of the lockfile.
func (l *Lockfile) Path() string { return l.path }

// fdOf narrows the uintptr file descriptor to the int expected by
// syscall.Flock. File descriptors on Unix are always small non-negative
// integers, so the conversion cannot overflow.
func fdOf(f *os.File) int {
	return int(f.Fd()) //nolint:gosec // G115: fd is a small non-negative integer
}

// Release unlocks and closes the lockfile. Safe to call multiple times;
// the second and later calls return nil.
func (l *Lockfile) Release() error {
	if l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	unlockErr := syscall.Flock(fdOf(f), syscall.LOCK_UN)
	closeErr := f.Close()
	switch {
	case unlockErr != nil:
		return fmt.Errorf("unlock %s: %w", l.path, unlockErr)
	case closeErr != nil:
		return fmt.Errorf("close %s: %w", l.path, closeErr)
	}
	return nil
}
