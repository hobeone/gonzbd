package unpack

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
func publishStagedExtraction(log *slog.Logger, outDir, stageDir string, opts Options) ([]string, error) {
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	stageBase := filepath.Base(stageDir)
	var published []string
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
