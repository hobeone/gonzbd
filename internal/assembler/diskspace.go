// Package assembler writes decoded NZB articles to target files using
// seek-based out-of-order assembly. A single worker goroutine serializes all
// disk I/O so that file-handle bookkeeping requires no locking. Articles are
// written with WriteAt (pwrite on Unix), which is idempotent at a given offset
// and requires no seek-before-write dance.
package assembler

import (
	"fmt"
	"syscall"
)

// FreeBytes returns the number of bytes available to unprivileged processes on
// the filesystem that contains dir. It uses syscall.Statfs, which is available
// on Linux, macOS, and other Unix-like systems.
//
// Windows is not implemented — this step is Linux/macOS-only. A build-tagged
// Windows implementation (using syscall.GetDiskFreeSpaceEx) can be added later
// without changing the API.
func FreeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("assembler: statfs %s: %w", dir, err)
	}
	// Bavail is the number of blocks available to unprivileged processes (uint64).
	// Bsize is the fundamental block size in bytes (int64 on Linux).
	// The product fits in int64 for any sane filesystem size.
	return int64(st.Bavail) * st.Bsize, nil //nolint:gosec // G115: Bavail is filesystem metadata, not user input
}
