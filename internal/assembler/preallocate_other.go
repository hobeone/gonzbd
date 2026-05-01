//go:build !linux

package assembler

import "os"

// preallocateFile pre-allocates size bytes for f using ftruncate.
// This creates a sparse file on filesystems that support it (NTFS,
// APFS, HFS+, ext4, xfs, btrfs). On filesystems without sparse
// support, this may allocate real blocks — acceptable because the
// file will be filled anyway.
func preallocateFile(f *os.File, size int64) error {
	if size <= 0 {
		return nil // nothing to pre-allocate
	}
	return f.Truncate(size)
}
