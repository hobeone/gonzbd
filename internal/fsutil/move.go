package fsutil

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// MoveFile moves src to dst. If os.Rename fails with a cross-device error
// (EXDEV), it falls back to copy+chmod+remove, preserving the source
// file's permissions. The fallback moves only a regular file: anything else,
// a symlink included, fails with ErrNotRegularFile and is left at src.
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
// indicates a cross-device rename failure (EXDEV).
func IsCrossDeviceError(err error) bool {
	return errors.Is(err, crossDeviceErr())
}

// IsRenameMergeNeeded reports whether err from os.Rename indicates that
// a file-by-file fallback move is required. This is true for both
// cross-device renames (EXDEV) and destination-not-empty renames
// (ENOTEMPTY / EEXIST), which occur when merging into existing dirs.
func IsRenameMergeNeeded(err error) bool {
	return IsCrossDeviceError(err) || errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

// ErrNotRegularFile is returned by MoveFile's cross-device fallback for a
// source that is not a regular file. A job holds no links
// (docs/post-processing-contract.md § "No links in a job"), so the fallback
// refuses a symlink rather than recreating it at the destination.
var ErrNotRegularFile = errors.New("not a regular file")

// copyAndRemove copies the regular file src to dst, preserving its mode, then
// removes src. Any other kind of source fails with ErrNotRegularFile before
// anything is created at dst. If the copy fails, any partial destination file
// is cleaned up before returning the error.
func copyAndRemove(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s (%s)", ErrNotRegularFile, src, info.Mode().Type())
	}

	mode := info.Mode()

	in, err := os.Open(src) //nolint:gosec // G304: path from caller, not user input
	if err != nil {
		return err
	}

	out, err := os.Create(dst) //nolint:gosec // G304: path from caller, not user input
	if err != nil {
		return errors.Join(err, in.Close())
	}

	// A cleanup failure below is joined to the error that triggered it, so a
	// partial dst left behind is reported rather than silent.
	if _, err = io.Copy(out, in); err != nil {
		return errors.Join(err, in.Close(), out.Close(), os.Remove(dst))
	}
	_ = in.Close() //nolint:errcheck // read-only input, and src is removed below regardless
	if err := out.Close(); err != nil {
		return errors.Join(err, os.Remove(dst))
	}
	if err := os.Chmod(dst, mode); err != nil {
		return errors.Join(err, os.Remove(dst))
	}
	return os.Remove(src)
}
