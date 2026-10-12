package unpack

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const unpackStagePrefix = ".gonzbd-tmp-unpack-"

// CleanupStageDirs removes any leftover .gonzbd-tmp-unpack-* staging
// directories from an earlier interrupted external extraction in outDir (#768).
func CleanupStageDirs(outDir string) {
	if outDir == "" {
		return
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), unpackStagePrefix) {
			_ = removeStageTree(filepath.Join(outDir, e.Name())) //nolint:errcheck // best effort; a stage left behind is retried by every later CleanupStageDirs
		}
	}
}

// removeStageDir removes an external extractor's staging directory once the
// extraction is over. A stage left behind sits inside outDir, which may be
// the job's output, until a later CleanupStageDirs, so a failure is logged.
func removeStageDir(log *slog.Logger, stageDir string) {
	if err := removeStageTree(stageDir); err != nil {
		log.Warn("unpack: failed to remove the staging directory", "dir", stageDir, "err", err)
	}
}

// removeStageTree removes stageDir and everything under it. A directory the
// extractor left without owner write or search permission stops
// os.RemoveAll, so on failure it grants the owner those bits and tries again
// (#862).
func removeStageTree(stageDir string) error {
	err := os.RemoveAll(stageDir)
	if err == nil {
		return nil
	}
	if ownErr := ownStageDirs(stageDir); ownErr != nil {
		return errors.Join(err, ownErr)
	}
	return os.RemoveAll(stageDir)
}

// ownStageDirs gives the owner read, write and search permission on stageDir
// and every directory under it. An external extractor restores the
// directory modes the archive stores, and a directory without owner write
// can have no entry renamed out of it or removed from it (#862). A directory
// is changed before its entries are read, so one without owner read or
// search is walked too.
func ownStageDirs(stageDir string) (err error) {
	root, err := os.OpenRoot(stageDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || !d.IsDir() {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if perm := info.Mode().Perm(); perm&0o700 != 0o700 {
			return root.Chmod(path, perm|0o700)
		}
		return nil
	})
}

// prepareStageDir cleans up any leftover .gonzbd-tmp-unpack-* directories in
// outDir and creates a fresh staging directory inside outDir (#768).
func prepareStageDir(outDir string) (string, error) {
	CleanupStageDirs(outDir)
	return os.MkdirTemp(outDir, unpackStagePrefix+"*")
}

// publishStagedExtraction atomically renames each completed file from stageDir
// into outDir after an external extractor (unrar or 7z) exits cleanly, and
// returns the relative paths of the published files (#768). When
// opts.OverwriteFiles is false, existing destination files in outDir are
// preserved and skipped.
//
// It enforces the "no links in a job" rule (see rar_links.go) on the
// external extractors' output: unrar can leave hard links in the stage (and
// symlinks, without -ol-), and 7z can leave symlinks. Only regular files are
// published. A file with a name outside the stage is not published at all;
// of several names for one inode, all inside the stage, only the first in
// walk (lexical) order is, so the surviving name of a hard-linked pair is not
// necessarily the archive's original member.
//
// A directory is created in outDir with 0o750 whatever mode the archive
// stored for it; the stage's own directories are first made owner-writable
// by ownStageDirs, so their files can be renamed out (#862).
func publishStagedExtraction(log *slog.Logger, outDir, stageDir string, opts Options) ([]string, error) {
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	if err := ownStageDirs(stageDir); err != nil {
		return nil, err
	}
	staged, err := countStagedNames(stageDir)
	if err != nil {
		return nil, err
	}
	stageBase := filepath.Base(stageDir)
	var published []string
	seen := make(map[fileID]struct{})
	err = filepath.WalkDir(stageDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == stageDir {
			return nil
		}
		rel, err := filepath.Rel(stageDir, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return root.MkdirAll(rel, 0o750)
		}
		if skip, err := isStagedLink(d, staged, seen); err != nil || skip {
			if skip {
				log.Warn("skipping link in extracted output: extraction creates no links in a job",
					"path", rel, "mode", d.Type())
				if opts.OnLine != nil {
					opts.OnLine("Skipping link: " + rel)
				}
			}
			return err
		}
		if !opts.OverwriteFiles {
			if _, err := root.Lstat(rel); err == nil {
				log.Info("skipping existing file", "path", filepath.Join(outDir, rel))
				if opts.OnLine != nil {
					opts.OnLine("Skipping existing: " + rel)
				}
				return nil
			}
		}
		if dir := filepath.Dir(rel); dir != "." {
			if err := root.MkdirAll(dir, 0o750); err != nil {
				return err
			}
		}
		if err := root.Rename(filepath.Join(stageBase, rel), rel); err != nil {
			return err
		}
		published = append(published, rel)
		return nil
	})
	return published, err
}

// fileID names an inode.
type fileID struct{ dev, ino uint64 }

// multiLinked returns the inode of info, whether it has more than one
// name and its link count. multi is false where the platform reports no link
// count.
func multiLinked(info os.FileInfo) (id fileID, nlink uint64, multi bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink <= 1 {
		return fileID{}, 0, false
	}
	return fileID{dev: uint64(st.Dev), ino: st.Ino}, uint64(st.Nlink), true //nolint:unconvert // Dev and Nlink widths vary by platform
}

// countStagedNames counts, for each regular file under stageDir with more
// than one name, how many of its names are under stageDir.
func countStagedNames(stageDir string) (map[fileID]uint64, error) {
	counts := make(map[fileID]uint64)
	err := filepath.WalkDir(stageDir, func(_ string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || !d.Type().IsRegular() {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if id, _, multi := multiLinked(info); multi {
			counts[id]++
		}
		return nil
	})
	return counts, err
}

// isStagedLink reports whether the non-directory stage entry d must not be
// published: anything but a regular file (a symlink above all); a regular
// file with a name outside the stage (fewer of its names in staged, the
// counts from countStagedNames, than its link count), which no name of may
// reach the job; or a regular file whose inode was already met under another
// name in seen. A first name is recorded in seen.
func isStagedLink(d os.DirEntry, staged map[fileID]uint64, seen map[fileID]struct{}) (bool, error) {
	if !d.Type().IsRegular() {
		return true, nil
	}
	info, err := d.Info()
	if err != nil {
		return false, err
	}
	id, nlink, multi := multiLinked(info)
	if !multi {
		return false, nil
	}
	if staged[id] < nlink {
		return true, nil
	}
	if _, dup := seen[id]; dup {
		return true, nil
	}
	seen[id] = struct{}{}
	return false, nil
}

// snapshotDir returns a set of all regular files (relative paths) under dir.
// Used to diff before/after extraction to determine which files were created.
func snapshotDir(dir string) (map[string]struct{}, error) {
	files := make(map[string]struct{})
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		files[rel] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// diffSnapshot returns files present in after but not in before (i.e. new files).
func diffSnapshot(before, after map[string]struct{}) []string {
	var newFiles []string
	for f := range after {
		if _, ok := before[f]; !ok {
			newFiles = append(newFiles, f)
		}
	}
	return newFiles
}
