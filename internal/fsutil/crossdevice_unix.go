//go:build unix

package fsutil

import "syscall"

// crossDeviceErr returns the sentinel error for cross-device rename failures.
func crossDeviceErr() error {
	return syscall.EXDEV
}
