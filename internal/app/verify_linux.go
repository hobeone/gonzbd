//go:build linux

package app

import (
	"os"

	"golang.org/x/sys/unix"
)

// dropPageCache asks the kernel to drop the file's cached pages, so the reads
// that follow come from the device rather than from pages marked clean after
// a failed writeback.
func dropPageCache(f *os.File) error {
	return unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}
