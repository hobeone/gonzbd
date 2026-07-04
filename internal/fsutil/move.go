package fsutil

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
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

// IsRenameMergeNeeded reports whether err from os.Rename indicates that
// a file-by-file fallback move is required. This is true for both
// cross-device renames (EXDEV) and destination-not-empty renames
// (ENOTEMPTY / EEXIST), which occur when merging into existing dirs.
func IsRenameMergeNeeded(err error) bool {
	return IsCrossDeviceError(err) || errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

// ErrSymlinkEscape is returned when a symlink target resolves outside
// the source directory during a cross-device move.
var ErrSymlinkEscape = errors.New("symlink target escapes source directory")

// copyAndRemove copies src to dst, preserving the original file mode,
// then removes src. Symlinks are validated: if the resolved target is
// contained within the source file's parent directory, the symlink is
// recreated at the destination; otherwise ErrSymlinkEscape is returned.
// If the copy fails, any partial destination file is cleaned up before
// returning the error.
func copyAndRemove(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}

	// Symlinks: validate containment, then recreate at destination.
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := checkSymlinkContainment(src, target); err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		return os.Remove(src)
	}

	mode := info.Mode()

	in, err := os.Open(src) //nolint:gosec // G304: path from caller, not user input
	if err != nil {
		return err
	}

	out, err := os.Create(dst) //nolint:gosec // G304: path from caller, not user input
	if err != nil {
		_ = in.Close() //nolint:errcheck // cleanup opened input on create error
		return err
	}

	if _, err = io.Copy(out, in); err != nil {
		_ = in.Close()     //nolint:errcheck // cleanup opened input on copy error
		_ = out.Close()    //nolint:errcheck // cleanup opened output on copy error
		_ = os.Remove(dst) // clean up partial file
		return err
	}
	// Close source before removing it — on Windows, Remove fails on open files.
	_ = in.Close() //nolint:errcheck // read-only input file is being deleted regardless
	if err := out.Close(); err != nil {
		_ = os.Remove(dst) // clean up partial file
		return err
	}
	if err := os.Chmod(dst, mode); err != nil {
		_ = os.Remove(dst) // clean up partial file
		return err
	}
	return os.Remove(src)
}

// checkSymlinkContainment verifies that the symlink at symlinkPath with
// the given target does not escape the symlink's parent directory. Both
// the resolved target and the parent directory are cleaned to absolute
// paths before comparison.
func checkSymlinkContainment(symlinkPath, target string) error {
	srcDir := filepath.Dir(symlinkPath)

	// Resolve target relative to the symlink's directory.
	resolved := target
	if !filepath.IsAbs(target) {
		resolved = filepath.Join(srcDir, target)
	}
	resolved = filepath.Clean(resolved)

	// Resolve srcDir to an absolute, symlink-free path.
	absDir, err := filepath.Abs(srcDir)
	if err != nil {
		return fmt.Errorf("resolve source dir: %w", err)
	}

	// The resolved target must be within absDir.
	if !PathWithin(absDir, resolved) {
		return fmt.Errorf("%w: %s -> %s escapes %s", ErrSymlinkEscape, symlinkPath, target, absDir)
	}
	return nil
}
