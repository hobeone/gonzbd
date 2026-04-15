//go:build windows

package sorting

import "syscall"

// crossDeviceErr returns the sentinel error for cross-device rename failures.
func crossDeviceErr() error {
	return syscall.ERROR_NOT_SAME_DEVICE
}
