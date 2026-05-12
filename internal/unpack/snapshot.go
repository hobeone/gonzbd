package unpack

import (
	"os"
	"path/filepath"
)

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
