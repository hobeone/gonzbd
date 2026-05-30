package deobfuscate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/par2"
)

// Par2Rename scans dir for .par2 files, builds a mapping of 16KB MD5 hashes
// to original filenames, and renames any obfuscated files that match.
func Par2Rename(ctx context.Context, log *slog.Logger, dir string, opts fsutil.SanitizeOptions) ([]Rename, error) {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "deobfuscate")

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("readdir %s: %w", dir, err)
	}

	// 1. Find all PAR2 files.
	var par2Files []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".par2") {
			par2Files = append(par2Files, filepath.Join(dir, e.Name()))
		}
	}

	if len(par2Files) == 0 {
		return nil, nil
	}

	// 2. Build hash-to-name map from all PAR2 files.
	hashToName := make(map[[16]byte]string)
	for _, p := range par2Files {
		descs, err := par2.ParseFileDescriptions(p)
		if err != nil {
			log.Warn("deobfuscate: failed to parse par2 file", "path", p, "err", err)
			continue
		}
		for _, d := range descs {
			hashToName[d.Hash16k] = d.FileName
		}
	}

	if len(hashToName) == 0 {
		return nil, nil
	}

	// 3. Scan regular files and hash their first 16KB.
	var renames []Rename
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		ext := strings.ToLower(filepath.Ext(e.Name()))

		// Skip if extension is in the excluded list or if it's a PAR2 file itself.
		if excludedExts[ext] || strings.EqualFold(ext, ".par2") {
			continue
		}

		// Calculate 16KB hash using the canonical par2 implementation.
		hash, err := par2.ComputeHash16k(path)
		if err != nil {
			log.Debug("deobfuscate: failed to hash file", "path", path, "err", err)
			continue
		}

		// Check if we have a match.
		if trueName, ok := hashToName[hash]; ok {
			if e.Name() == trueName {
				continue
			}

			// Perform rename; GetUniqueFilename adds a numeric suffix if the
			// par2-recorded target already exists on disk.
			desiredPath := fsutil.JoinSafe(dir, "", trueName, opts)
			newPath := fsutil.GetUniqueFilename(desiredPath)

			if newPath != desiredPath {
				// The par2 target already exists. Check whether it has the same
				// content as the obfuscated file. If so, the obfuscated copy is a
				// redundant duplicate — delete it rather than producing a .1 file.
				identical, cerr := contentEqual(path, desiredPath, hash)
				if cerr != nil {
					log.Debug("deobfuscate: content check failed, keeping both files",
						"obfuscated", e.Name(), "existing", trueName, "err", cerr)
				} else if identical {
					if rerr := os.Remove(path); rerr != nil {
						log.Warn("deobfuscate: failed to remove duplicate obfuscated file",
							"path", path, "err", rerr)
					} else {
						log.Info("deobfuscate: deleted duplicate obfuscated file",
							"deleted", e.Name(),
							"identical_to", trueName,
						)
					}
					continue
				} else {
					log.Warn("deobfuscate: par2 target already exists with different content — renamed to avoid overwriting",
						"obfuscated", e.Name(),
						"par2_name", trueName,
						"renamed_to", filepath.Base(newPath),
					)
				}
			}

			r, err := renameRecorded(log, path, newPath, trueName, "deobfuscate: par2-renamed")
			if err != nil {
				return renames, fmt.Errorf("rename %s → %s: %w", path, newPath, err)
			}
			renames = append(renames, r)
		}
	}

	return renames, nil
}
