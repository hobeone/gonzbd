//go:build linux

package assembler

import (
	"os"

	"golang.org/x/sys/unix"
)

// preallocateFile pre-allocates size bytes for f using fallocate(2).
// On ext4/xfs this reserves contiguous extents without zeroing,
// eliminating per-write extent-tree updates. Falls back to ftruncate
// (sparse file) if fallocate is not supported by the filesystem.
func preallocateFile(f *os.File, size int64) error {
	if size <= 0 {
		return nil // nothing to pre-allocate
	}
	//nolint:gosec // G115: file descriptor fits in int
	err := unix.Fallocate(int(f.Fd()), 0, 0, size)
	if err == nil {
		return nil
	}
	// ENOTSUP / EOPNOTSUPP: filesystem doesn't support fallocate
	// (e.g. NFS, tmpfs, older FUSE mounts). Fall back to ftruncate
	// which creates a sparse file on most filesystems.
	if err == unix.ENOTSUP || err == unix.EOPNOTSUPP {
		return f.Truncate(size)
	}
	return err
}
