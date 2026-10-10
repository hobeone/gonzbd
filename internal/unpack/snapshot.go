package unpack

import (
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
			_ = os.RemoveAll(filepath.Join(outDir, e.Name()))
		}
	}
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
// published, and of several names for one inode only the first in walk
// (lexical) order is, so the surviving name of a hard-linked pair is not
// necessarily the archive's original member.
func publishStagedExtraction(log *slog.Logger, outDir, stageDir string, opts Options) ([]string, error) {
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

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
			info, err := d.Info()
			if err != nil {
				return err
			}
			return root.MkdirAll(rel, info.Mode().Perm())
		}
		if skip, err := isStagedLink(d, seen); err != nil || skip {
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

// isStagedLink reports whether the non-directory stage entry d must not be
// published: anything but a regular file (a symlink above all), or a regular
// file whose inode was already met under another name in seen. A first name
// is recorded in seen.
func isStagedLink(d os.DirEntry, seen map[fileID]struct{}) (bool, error) {
	if !d.Type().IsRegular() {
		return true, nil
	}
	info, err := d.Info()
	if err != nil {
		return false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink <= 1 {
		return false, nil
	}
	id := fileID{dev: uint64(st.Dev), ino: st.Ino} //nolint:unconvert // Dev is int32 on some platforms
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
