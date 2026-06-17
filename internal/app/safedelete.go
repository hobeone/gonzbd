package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// safeDeleteDir recursively removes targetDir, but only if it is contained
// within one of the provided baseDirs. It is defense-in-depth against a crafted
// or corrupted job name / stored path escaping the download or complete tree.
//
// It is a no-op (returns nil) when targetDir is empty or no non-empty base is
// supplied. It returns an error — and deletes nothing — when targetDir escapes
// every base, or when it equals any base (refusing to delete a configured root).
func safeDeleteDir(targetDir string, baseDirs ...string) error {
	if targetDir == "" {
		return nil
	}
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return err
	}
	anyBase := false
	for _, base := range baseDirs {
		if base == "" {
			continue
		}
		anyBase = true
		absBase, err := filepath.Abs(base)
		if err != nil {
			return err
		}
		if absBase == absTarget {
			return fmt.Errorf("refusing to delete base directory %q", absBase)
		}
		if fsutil.PathWithin(absBase, absTarget) {
			return os.RemoveAll(absTarget)
		}
	}
	if !anyBase {
		return nil
	}
	return fmt.Errorf("containment breach: %q is not inside any of %v", absTarget, baseDirs)
}
