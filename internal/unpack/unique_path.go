package unpack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// uniquePath returns a path that doesn't conflict with existing files.
// If destPath doesn't exist, it's returned unchanged. Otherwise a numeric
// suffix is appended before the extension: "file.txt" → "file_1.txt",
// "file_1.txt" → "file_2.txt", etc. This mirrors 7z's -aou behavior.
func uniquePath(destPath string) string {
	if _, err := os.Stat(destPath); err != nil {
		return destPath // doesn't exist, use as-is
	}

	dir := filepath.Dir(destPath)
	base := filepath.Base(destPath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)

	for i := 1; i < 10000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s_%d%s", name, i, ext))
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}

	// Exhausted 10000 candidates — extremely unlikely in practice.
	return destPath
}
