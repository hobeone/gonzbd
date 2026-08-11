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
	syscall.EIO,     // hardware or filesystem-level I/O error
	syscall.ENOENT,  // target directory removed underneath us
	syscall.ENOTDIR, // a path component is not a directory
	syscall.EACCES,  // permissions
	syscall.EPERM,
	syscall.EBADF,  // programming error: handle already closed
	syscall.EFBIG,  // exceeds the filesystem's maximum file size
	syscall.EINVAL, // bad offset or unaligned write
}

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
