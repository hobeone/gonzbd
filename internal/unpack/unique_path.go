package unpack

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// uniquePath returns a relative name under root that doesn't conflict with an
// existing entry. If rel is free it comes back unchanged. Otherwise a numeric
// suffix is appended before the extension: "file.txt" → "file_1.txt",
// "file_1.txt" → "file_2.txt", etc.
//
// The "_N" form rather than fsutil.GetUniqueRelPath's ".N" is deliberate: this
// mirrors 7z's -aou behaviour, and the names it produces are what a user sees
// after a OneFolder extraction.
//
// It probes THROUGH root rather than calling os.Lstat on an absolute path.
// Lstat does not follow a symlink at the final component, but the OS still
// resolves one in every leading component, so an absolute probe answers
// questions about locations outside the extraction root — an existence oracle
// for the host filesystem — and decides this name on state the write itself can
// never reach. Confining the probe makes the question the same one the write
// will ask.
func uniquePath(root *os.Root, rel string) string {
	if nameIsFree(root, rel) {
		return rel
	}

	dir := filepath.Dir(rel)
	base := filepath.Base(rel)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)

	for i := 1; i < 10000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s_%d%s", name, i, ext))
		if nameIsFree(root, candidate) {
			return candidate
		}
	}

	// Exhausted 10000 candidates — extremely unlikely in practice.
	return rel
}

// nameIsFree reports whether nothing under root holds rel.
//
// Only ErrNotExist means free. Any other error means the name could not be
// shown to be unoccupied, and reading EACCES as "available" is how a name that
// cannot even be probed becomes a write attempt. This matches
// fsutil.GetUniqueRelPath, which treats a non-ErrNotExist error as occupied for
// the same reason.
//
// Lstat, not Stat, for the reason given on fsutil.GetUniqueRelPath.
func nameIsFree(root *os.Root, rel string) bool {
	_, err := root.Lstat(rel)
	return errors.Is(err, os.ErrNotExist)
}
