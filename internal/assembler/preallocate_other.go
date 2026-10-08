//go:build !linux

package assembler

import "os"

// preallocateFile is a no-op on non-Linux platforms, which have no portable
// fallocate(2) equivalent that reserves physical extents without extending
// i_size via ftruncate.
func preallocateFile(_ *os.File, _ int64) error { //nocover: non-Linux stub, not compiled on Linux CI
	return nil
}
