package fsutil

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteAtomic writes data produced by write() to path atomically:
// temp file (sibling to path) → write → fsync → close → rename.
//
// A crash at any point leaves either the original file or the new file
// intact — never a half-written mix. The caller is responsible for
// ensuring the parent directory of path exists before calling.
//
// write() receives an *os.File wrapped as io.Writer. If write() returns
// an error the temp file is removed and the error is returned; the
// target file is untouched.
func WriteAtomic(path string, write func(w io.Writer) error) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	tmp, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return fmt.Errorf("atomic write %s: create temp: %w", base, err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()        // cleanup temp file on error path
		_ = os.Remove(tmpName) // cleanup temp file on error path
	}

	if err := write(tmp); err != nil {
		cleanup()
		return fmt.Errorf("atomic write %s: %w", base, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("atomic write %s: fsync: %w", base, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) // cleanup temp file on close error
		return fmt.Errorf("atomic write %s: close temp: %w", base, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName) // cleanup temp file on rename error
		return fmt.Errorf("atomic write %s: rename: %w", base, err)
	}
	return nil
}

// WriteAtomicBytes writes data to path atomically. It is a convenience
// wrapper around WriteAtomic for the common case of writing a plain
// byte slice.
func WriteAtomicBytes(path string, data []byte) error {
	return WriteAtomic(path, func(w io.Writer) error {
		_, err := io.Copy(w, bytes.NewReader(data))
		return err
	})
}
