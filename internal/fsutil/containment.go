package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheckContainment walks dir and verifies that every file and symlink
// target resolves to a path inside dir. This catches directory traversal
// attacks where an archive contains entries like "../../../etc/crontab"
// that extract outside the intended output directory.
//
// Returns an error listing the first offending path if any entry escapes
// dir. Returns nil if every entry is safely contained.
func CheckContainment(dir string) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("containment: resolve dir: %w", err)
	}
	// Resolve any symlinks in the base directory itself so that
	// comparisons are against the canonical path.
	absDir, err = filepath.EvalSymlinks(absDir)
	if err != nil {
		return fmt.Errorf("containment: eval symlinks on dir: %w", err)
	}

	return filepath.WalkDir(absDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Skip the root directory itself.
		if path == absDir {
			return nil
		}

		// Resolve the real path, following symlinks.
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			// If the target doesn't exist, that's still suspicious.
			return fmt.Errorf("containment: eval symlinks %s: %w", path, err)
		}
		realAbs, err := filepath.Abs(realPath)
		if err != nil {
			return fmt.Errorf("containment: abs %s: %w", realPath, err)
		}

		// The resolved path must be within absDir.
		if !PathWithin(absDir, realAbs) {
			return fmt.Errorf("containment: path %q resolves to %q which is outside %q", path, realAbs, absDir)
		}
		return nil
	})
}

// PathWithin returns true if target is within or equal to base.
// It resolves relative paths (using filepath.Clean) and checks that
// target does not escape base via ".." sequences.
func PathWithin(base, target string) bool {
	// First, clean both paths.
	baseClean := filepath.Clean(base)
	targetClean := filepath.Clean(target)

	// Make both absolute if possible to handle mixing of absolute and relative paths.
	if absBase, err := filepath.Abs(baseClean); err == nil {
		if absTarget, err := filepath.Abs(targetClean); err == nil {
			baseClean = absBase
			targetClean = absTarget
		}
	}

	rel, err := filepath.Rel(baseClean, targetClean)
	if err != nil {
		return false
	}
	// Rel returns "." if they are equal, or a path starting with ".." if target is outside.
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
