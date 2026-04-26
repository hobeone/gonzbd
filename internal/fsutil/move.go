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

// IsCrossDeviceError reports whether err (or any error in its chain)
// indicates a cross-device rename failure (EXDEV on Unix,
// ERROR_NOT_SAME_DEVICE on Windows).
func IsCrossDeviceError(err error) bool {
	return errors.Is(err, crossDeviceErr())
}

// copyAndRemove copies src to dst, preserving the original file mode,
// then removes src. If the copy fails, any partial destination file is
// cleaned up before returning the error.
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
		os.Remove(dst) // clean up partial file
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst) // clean up partial file
		return err
	}
	if err := os.Chmod(dst, mode); err != nil {
		os.Remove(dst) // clean up partial file
		return err
	}
	return os.Remove(src)
}
