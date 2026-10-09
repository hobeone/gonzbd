//go:build !linux

package app

import "os"

// dropPageCache is a no-op where posix_fadvise is not available through
// golang.org/x/sys/unix; the fsync before it still reports an unreported
// writeback error.
func dropPageCache(_ *os.File) error { //nocover: non-Linux stub, not compiled on Linux CI
	return nil
}
