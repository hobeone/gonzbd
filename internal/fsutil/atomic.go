package fsutil

import (
	"bytes"
	"compress/gzip"
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
	return writeAtomic(path, 0, false, write)
}

// WriteAtomicPerm is like WriteAtomic but chmods the temp file to perm
// before the rename, so the permissions of the published file are correct
// from the moment it becomes visible (never a window at the default
// os.CreateTemp mode).
func WriteAtomicPerm(path string, perm os.FileMode, write func(w io.Writer) error) error {
	return writeAtomic(path, perm, true, write)
}

func writeAtomic(path string, perm os.FileMode, chmod bool, write func(w io.Writer) error) error {
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
	if chmod {
		if err := os.Chmod(tmpName, perm); err != nil {
			_ = os.Remove(tmpName) // cleanup temp file on chmod error
			return fmt.Errorf("atomic write %s: chmod: %w", base, err)
		}
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

// WriteAtomicBytesPerm writes data to path atomically with the given
// permissions applied before the file is published. Convenience wrapper
// around WriteAtomicPerm for the common case of writing a plain byte slice.
func WriteAtomicBytesPerm(path string, data []byte, perm os.FileMode) error {
	return WriteAtomicPerm(path, perm, func(w io.Writer) error {
		_, err := io.Copy(w, bytes.NewReader(data))
		return err
	})
}

// WriteGzAtomic gzip-compresses data produced by write() and atomically
// publishes it at path using the same temp-file → fsync → close → rename
// sequence as WriteAtomic. write() receives an io.Writer that gzips
// everything written to it; the gzip stream is closed (flushing its
// trailer) before the underlying temp file is synced.
func WriteGzAtomic(path string, write func(gz io.Writer) error) error {
	return WriteAtomic(path, func(w io.Writer) error {
		gz := gzip.NewWriter(w)
		if err := write(gz); err != nil {
			_ = gz.Close() // cleanup gzip writer on write error
			return err
		}
		return gz.Close()
	})
}

// WriteGzAtomicBytes gzip-compresses data and atomically publishes it at
// path. Convenience wrapper around WriteGzAtomic for the common case of
// compressing a plain byte slice.
func WriteGzAtomicBytes(path string, data []byte) error {
	return WriteGzAtomic(path, func(gz io.Writer) error {
		_, err := gz.Write(data)
		return err
	})
}
