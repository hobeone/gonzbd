package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// safeDeleteDir recursively removes targetDir, but only through an os.Root
// anchored at one of the provided baseDirs. Going through the rooted handle
// makes the removal structurally unable to escape that base via "..", an
// absolute path, or a symlinked path component, and collapses validation and
// deletion into a single rooted operation (no check-then-act TOCTOU window).
// It is defense-in-depth against a crafted/corrupted job name or stored path.
//
// It is a no-op (returns nil) when targetDir is empty or no non-empty base is
// supplied. It returns an error — deleting nothing — when targetDir escapes
// every base, or equals any base (refusing to delete a configured root).
//
// baseDirs are trusted configuration roots; os.OpenRoot resolves symlinks in
// the base name itself, after which all operations are confined to the root.
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
		rel, err := filepath.Rel(absBase, absTarget)
		if err != nil || !filepath.IsLocal(rel) {
			continue // target is not lexically within this base; try the next
		}
		root, err := os.OpenRoot(absBase)
		if err != nil {
			return err
		}
		// RemoveRootAll is performed relative to the root; the runtime refuses any
		// component that would traverse a symlink or escape the root. Close
		// explicitly (not via defer) since we return inside the loop.
		rmErr := fsutil.RemoveRootAll(root, rel, absTarget)
		_ = root.Close() // best-effort; deletion already attempted
		return rmErr
	}
	if !anyBase {
		return nil
	}
	return fmt.Errorf("containment breach: %q is not inside any of %v", absTarget, baseDirs)
}
