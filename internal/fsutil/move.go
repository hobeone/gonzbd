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
// (EXDEV), it falls back to copy+chmod+remove, preserving the source
// file's permissions.
//
// A symlink moved by the fallback is checked against src's own directory;
// use MoveFileWithin when src sits inside a larger tree that is being moved.
func MoveFile(src, dst string) error {
	return MoveFileWithin(filepath.Dir(src), src, dst)
}

// MoveFileWithin is MoveFile for a src that belongs to the tree rooted at
// root. On the cross-device fallback a symlink is recreated at dst only if its
// target stays inside root, which is what keeps a valid "../lib/x" link
// between sibling directories of one job from failing the move.
func MoveFileWithin(root, src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, crossDeviceErr()) {
		return err
	}
	// Cross-device fallback: copy + preserve permissions + remove original.
	return copyAndRemoveWithin(root, src, dst)
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

// ErrSymlinkEscape is returned when a symlink target resolves outside
// the source directory during a cross-device move.
var ErrSymlinkEscape = errors.New("symlink target escapes source directory")

// copyAndRemove is copyAndRemoveWithin rooted at src's parent directory.
func copyAndRemove(src, dst string) error {
	return copyAndRemoveWithin(filepath.Dir(src), src, dst)
}

// copyAndRemoveWithin copies src to dst, preserving the original file mode,
// then removes src. Symlinks are validated: if the resolved target is
// contained within root, the symlink is recreated at the destination;
// otherwise ErrSymlinkEscape is returned.
// If the copy fails, any partial destination file is cleaned up before
// returning the error.
func copyAndRemoveWithin(root, src, dst string) error {
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
		if err := checkSymlinkContainmentWithin(root, src, target); err != nil {
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
		_ = in.Close() // cleanup opened input on create error
		return err
	}

	if _, err = io.Copy(out, in); err != nil {
		_ = in.Close()     // cleanup opened input on copy error
		_ = out.Close()    // cleanup opened output on copy error
		_ = os.Remove(dst) // clean up partial file
		return err
	}
	// Close source before removing it — on Windows, Remove fails on open files.
	_ = in.Close() // read-only input file is being deleted regardless
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

// checkSymlinkContainment is checkSymlinkContainmentWithin rooted at the
// symlink's own parent directory.
func checkSymlinkContainment(symlinkPath, target string) error {
	return checkSymlinkContainmentWithin(filepath.Dir(symlinkPath), symlinkPath, target)
}

// checkSymlinkContainmentWithin verifies that the symlink at symlinkPath with
// the given target does not escape root. The target is resolved against the
// symlink's own directory; both it and root are cleaned to absolute paths
// before comparison.
func checkSymlinkContainmentWithin(root, symlinkPath, target string) error {
	// Resolve target relative to the symlink's directory.
	resolved := target
	if !filepath.IsAbs(target) {
		resolved = filepath.Join(filepath.Dir(symlinkPath), target)
	}
	resolved = filepath.Clean(resolved)

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve root dir: %w", err)
	}

	// The resolved target must be within absRoot.
	if !PathWithin(absRoot, resolved) {
		return fmt.Errorf("%w: %s -> %s escapes %s", ErrSymlinkEscape, symlinkPath, target, absRoot)
	}
	return nil
}
