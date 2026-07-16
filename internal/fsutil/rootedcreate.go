package fsutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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

// RootedCreateTemp creates a uniquely-named temporary file in the same
// directory as rel (so a subsequent root.Rename to rel is same-filesystem
// and atomic), through a rooted directory handle. It calls root.MkdirAll for
// the parent directory, then retries root.OpenFile with a random suffix on
// name collision (mirroring os.CreateTemp's own retry behavior, since
// os.Root has no CreateTemp of its own).
//
// It returns the open file and the relative path it was created at (which
// the caller must root.Rename into place on success, or root.Remove on
// failure — RootedCreateTemp itself does not clean up). The caller is
// responsible for closing the returned *os.File.
func RootedCreateTemp(root *os.Root, rel string) (*os.File, string, error) {
	dir := filepath.Dir(rel)
	if dir != "." {
		if err := root.MkdirAll(dir, 0o750); err != nil {
			return nil, "", fmt.Errorf("fsutil: mkdir %s: %w", dir, err)
		}
	}
	base := filepath.Base(rel)
	const maxAttempts = 10_000
	for range maxAttempts {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, "", fmt.Errorf("fsutil: generate temp suffix: %w", err)
		}
		tmpRel := filepath.Join(dir, base+".gonzbd-tmp-"+hex.EncodeToString(buf[:]))
		f, err := root.OpenFile(tmpRel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, tmpRel, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", fmt.Errorf("fsutil: create temp for %s: %w", rel, err)
		}
	}
	return nil, "", fmt.Errorf("fsutil: create temp for %s: exhausted %d attempts", rel, maxAttempts)
}

// GetUniqueRelPath returns a unique version of rel relative to root by checking
// existence using root.Stat. It appends .1, .2, etc., up to 10,000 attempts if the
// target already exists.
func GetUniqueRelPath(root *os.Root, rel string) string {
	if _, err := root.Stat(rel); err != nil {
		// If the file definitely doesn't exist relative to root, use this path.
		if errors.Is(err, os.ErrNotExist) {
			return rel
		}
		// If it's a permission error (or other OS error), the path is occupied/blocked.
		// Fall through to the loop to check if we can create a suffix (e.g. rel.1).
	}
	ext := filepath.Ext(rel)
	base := rel[:len(rel)-len(ext)]
	for i := 1; i <= 10_000; i++ {
		newRel := fmt.Sprintf("%s.%d%s", base, i, ext)
		if _, err := root.Stat(newRel); err != nil {
			// If the suffix path doesn't exist, we can use it.
			if errors.Is(err, os.ErrNotExist) {
				return newRel
			}
			// If we get a permission error on the suffix path, the root/parent directory
			// is likely inaccessible. Stop looping immediately to prevent 10,000 futile syscalls.
			return newRel
		}
	}
	return fmt.Sprintf("%s.%d%s", base, 10_000, ext)
}
