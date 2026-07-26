package fsutil

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// removeAllFunc and removeFunc are package-level hooks for unit testing
// error simulation (e.g., EBUSY on NFS silly-rename races).
var (
	removeAllFunc     = os.RemoveAll
	removeFunc        = os.Remove
	rootRemoveAllFunc = func(r *os.Root, name string) error { return r.RemoveAll(name) }
)

// IsSillyRenameFile reports whether filename is an NFS, SMB, or FUSE
// "silly rename" artifact created when an open file is deleted.
func IsSillyRenameFile(name string) bool {
	base := filepath.Base(name)
	return strings.HasPrefix(base, ".nfs") ||
		strings.HasPrefix(base, ".smbdelete") ||
		strings.HasPrefix(base, ".fuse_hidden")
}

// ContainsOnlySillyRenames checks if dir contains only directory structures
// and silly-rename files (.nfs*, .smbdelete*, .fuse_hidden*). Returns true
// if at least one silly-rename file is present and zero normal files exist.
func ContainsOnlySillyRenames(dir string) (onlySilly bool, sillyFiles []string, err error) {
	hasNormalFile := false

	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if IsSillyRenameFile(d.Name()) {
			sillyFiles = append(sillyFiles, path)
		} else {
			hasNormalFile = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return false, nil, err
	}
	return !hasNormalFile && len(sillyFiles) > 0, sillyFiles, nil
}

// isBusyOrNotEmpty reports whether err indicates a temporary file-locking
// or directory-not-empty condition common during NFS/SMB operations.
func isBusyOrNotEmpty(err error) bool {
	return errors.Is(err, syscall.EBUSY) ||
		errors.Is(err, syscall.ENOTEMPTY) ||
		errors.Is(err, syscall.EEXIST) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EACCES)
}

// RemoveAll is like os.RemoveAll, but implements a 2-tier protocol for
// network filesystems (NFS, SMB) and Windows:
//  1. Retries up to 5 times with exponential backoff on EBUSY/ENOTEMPTY.
//  2. If deletion still fails, checks if the remaining items are solely
//     silly-rename files (.nfs*). If so, logs an informational warning and
//     returns nil so pipeline stages succeed without error.
func RemoveAll(path string) error {
	var lastErr error
	backoffs := []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}

	for i := 0; i <= len(backoffs); i++ {
		err := removeAllFunc(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		lastErr = err
		if !isBusyOrNotEmpty(err) || i == len(backoffs) {
			break
		}
		time.Sleep(backoffs[i])
	}

	// Tier 2: Check if failure is solely due to lingering NFS/SMB silly renames.
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		if ok, silly, _ := ContainsOnlySillyRenames(path); ok {
			slog.Warn("directory removal delayed: lingering NFS/SMB silly-rename files detected",
				"path", path,
				"silly_files_count", len(silly),
				"err", lastErr)
			return nil
		}
	} else if IsSillyRenameFile(path) {
		slog.Warn("file removal delayed: lingering NFS/SMB silly-rename file detected",
			"path", path,
			"err", lastErr)
		return nil
	}

	return fmt.Errorf("fsutil.RemoveAll %q: %w", path, lastErr)
}

// Remove is like os.Remove, retrying on temporary EBUSY/EACCES errors and
// ignoring persistent failures on individual silly-rename files.
func Remove(path string) error {
	var lastErr error
	backoffs := []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}

	for i := 0; i <= len(backoffs); i++ {
		err := removeFunc(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		lastErr = err
		if !isBusyOrNotEmpty(err) || i == len(backoffs) {
			break
		}
		time.Sleep(backoffs[i])
	}

	if IsSillyRenameFile(path) {
		slog.Warn("file removal delayed: lingering NFS/SMB silly-rename file detected",
			"path", path,
			"err", lastErr)
		return nil
	}

	return fmt.Errorf("fsutil.Remove %q: %w", path, lastErr)
}

// RemoveRootAll is like root.RemoveAll(rel), but implements the 2-tier
// protocol for network filesystems (retrying on EBUSY/ENOTEMPTY and
// ignoring lingering silly-rename files).
func RemoveRootAll(root *os.Root, rel, fullPath string) error {
	var lastErr error
	backoffs := []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}

	for i := 0; i <= len(backoffs); i++ {
		err := rootRemoveAllFunc(root, rel)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		lastErr = err
		if !isBusyOrNotEmpty(err) || i == len(backoffs) {
			break
		}
		time.Sleep(backoffs[i])
	}

	if info, err := os.Stat(fullPath); err == nil && info.IsDir() {
		if ok, silly, _ := ContainsOnlySillyRenames(fullPath); ok {
			slog.Warn("directory removal delayed: lingering NFS/SMB silly-rename files detected",
				"path", fullPath,
				"silly_files_count", len(silly),
				"err", lastErr)
			return nil
		}
	} else if IsSillyRenameFile(fullPath) {
		slog.Warn("file removal delayed: lingering NFS/SMB silly-rename file detected",
			"path", fullPath,
			"err", lastErr)
		return nil
	}

	return fmt.Errorf("fsutil.RemoveRootAll %q: %w", fullPath, lastErr)
}
