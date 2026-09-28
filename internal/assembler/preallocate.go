package assembler

import "os"

// growFile extends f to size bytes via ftruncate, but never shrinks it.
//
// A reopened partial file may already exceed FileInfo.ExpectedSize:
// offsetOutOfRange allows writes up to ExpectedSize/offsetSlackDivisor past
// it. Both platform implementations of preallocateFile — preallocate_linux.go's
// ENOTSUP/EOPNOTSUPP fallback, and preallocate_other.go's only mechanism —
// call this rather than truncating directly.
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
