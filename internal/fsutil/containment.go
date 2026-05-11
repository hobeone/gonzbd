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
	// Ensure trailing separator for prefix matching.
	prefix := absDir + string(filepath.Separator)

	return filepath.WalkDir(absDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Skip the root directory itself.
		if path == absDir {
			return nil
		}

		// Resolve the real path, following symlinks.
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			// If the target doesn't exist, that's still suspicious.
			return fmt.Errorf("containment: eval symlinks %s: %w", path, err)
		}
		realAbs, err := filepath.Abs(real)
		if err != nil {
			return fmt.Errorf("containment: abs %s: %w", real, err)
		}

		// The resolved path must be exactly absDir or start with absDir + separator.
		if realAbs != absDir && !strings.HasPrefix(realAbs, prefix) {
			return fmt.Errorf("containment: path %q resolves to %q which is outside %q", path, realAbs, absDir)
		}
		return nil
	})
}
