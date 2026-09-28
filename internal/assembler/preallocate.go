package assembler

import "os"

// growFile extends f to size bytes via ftruncate, but never shrinks it below
// its current size. It is the ftruncate-based path shared by both platform
// implementations of preallocateFile: preallocate_linux.go's
// ENOTSUP/EOPNOTSUPP fallback, and preallocate_other.go's only mechanism.
//
// Pre-allocation exists to reserve space ahead of writes, not to pin a file
// at exactly size — and a target file can legitimately already be larger
// than FileInfo.ExpectedSize when this runs. openTargetFile calls
// preallocateFile on every first open in an open episode, including
// reopening an existing partial after a restart, and offsetOutOfRange
// accepts writes up to 1+1/offsetSlackDivisor of ExpectedSize (#388). An
// unconditional ftruncate(size) on that reopen would discard the tail of an
// already-larger file, silently losing bytes a resume may have already
// verified and marked Done.
func growFile(f *os.File, size int64) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() >= size {
		return nil
	}
	return f.Truncate(size)
}
