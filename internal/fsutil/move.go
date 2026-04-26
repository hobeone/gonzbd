package fsutil

import (
	"errors"
	"io"
	"os"
)

// MoveFile moves src to dst. If os.Rename fails with a cross-device error
// (EXDEV on Unix, ERROR_NOT_SAME_DEVICE on Windows), it falls back to
// copy+chmod+remove, preserving the source file's permissions.
func MoveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, crossDeviceErr()) {
		return err
	}
	// Cross-device fallback: copy + preserve permissions + remove original.
	return copyAndRemove(src, dst)
}

// copyAndRemove copies src to dst, preserving the original file mode,
// then removes src.
func copyAndRemove(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	mode := info.Mode()

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, mode); err != nil {
		return err
	}
	return os.Remove(src)
}
