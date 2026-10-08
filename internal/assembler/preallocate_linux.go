//go:build linux

package assembler

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// preallocateFile pre-allocates size bytes for f using fallocate(2).
// On ext4/xfs/btrfs this reserves contiguous extents without zeroing,
// eliminating per-write extent-tree updates. When the filesystem does not
// support fallocate (ENOTSUP/EOPNOTSUPP, e.g. NFS before v4.2 or without
// server ALLOCATE support, ZFS, or older FUSE mounts), pre-allocation is
// skipped rather than falling back to ftruncate: ftruncate reserves no
// physical extents on sparse filesystems, costs an extra Stat+Truncate
// metadata round-trip, and WriteAt at an arbitrary offset already creates
// sparse holes.
func preallocateFile(f *os.File, size int64) error {
	if size <= 0 {
		return nil // nothing to pre-allocate
	}
	//nolint:gosec // G115: file descriptor fits in int
	err := unix.Fallocate(int(f.Fd()), 0, 0, size)
	if err == nil || errors.Is(err, unix.EOPNOTSUPP) {
		return nil
	}
	return err
}
