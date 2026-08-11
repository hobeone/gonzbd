package storagefault

import (
	"errors"
	"syscall"
)

// permanentErrnos are conditions no amount of waiting will clear. Anything
// not listed is treated as retryable, which is the conservative default: a
// stalled job is recoverable by the user, whereas failing a job discards
// work that a transient mount hiccup would have let us keep.
var permanentErrnos = []syscall.Errno{
	syscall.EROFS,   // read-only filesystem
	syscall.ENOTDIR, // a path component is not a directory
	syscall.EACCES,  // permissions
	syscall.EPERM,
	syscall.EBADF,  // programming error: handle already closed
	syscall.EFBIG,  // exceeds the filesystem's maximum file size
	syscall.EINVAL, // bad offset or unaligned write
}

// EIO and ENOENT are deliberately absent. Both look permanent and are not:
// a mid-write EIO on a network-backed or removable volume is frequently
// transient, and a missing target directory is the most cheaply recoverable
// storage condition there is. Retryable here means the job stalls with a
// surfaced reason (R19), not that anything retries forever — so the operator
// still sees a dying disk, while a transient fault no longer discards every
// byte the job had already downloaded. See R18.

// Classify maps an error from a storage operation to a Fault. It returns nil
// when err is nil, so callers can write `if f := Classify(...); f != nil`.
func Classify(op, path string, err error) *Fault {
	if err == nil {
		return nil
	}
	f := &Fault{Op: op, Path: path, Err: err}
	for _, e := range permanentErrnos {
		if errors.Is(err, e) {
			f.Permanent = true
			break
		}
	}
	return f
}
