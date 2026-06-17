package fsutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// RootedOpenFile creates parent directories and opens a file through a rooted
// directory handle. It calls root.MkdirAll for the parent directory, then
// root.OpenFile with the supplied flag and perm, both relative to root.
//
// Using an os.Root handle means the operation cannot escape the root directory
// via "..", an absolute path, or a symlinked path component — even if the
// caller's lexical sanitization is bypassed. This is defense-in-depth for
// archive extraction (zip-slip prevention).
//
// rel must be a path relative to root's base directory (i.e. the output from
// SanitizeArchivePath). The caller is responsible for closing the returned
// *os.File.
func RootedOpenFile(root *os.Root, rel string, flag int, perm fs.FileMode) (*os.File, error) {
	dir := filepath.Dir(rel)
	if dir != "." {
		if err := root.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("fsutil: mkdir %s: %w", dir, err)
		}
	}
	f, err := root.OpenFile(rel, flag, perm)
	if err != nil {
		return nil, fmt.Errorf("fsutil: open %s: %w", rel, err)
	}
	return f, nil
}

// GetUniqueRelPath returns a unique version of rel relative to root by checking
// existence using root.Stat. It appends .1, .2, etc., up to 10,000 attempts if the
// target already exists.
func GetUniqueRelPath(root *os.Root, rel string) string {
	if _, err := root.Stat(rel); err != nil {
		return rel
	}
	ext := filepath.Ext(rel)
	base := rel[:len(rel)-len(ext)]
	for i := 1; i <= 10_000; i++ {
		newRel := fmt.Sprintf("%s.%d%s", base, i, ext)
		if _, err := root.Stat(newRel); err != nil {
			return newRel
		}
	}
	return fmt.Sprintf("%s.%d%s", base, 10_000, ext)
}
