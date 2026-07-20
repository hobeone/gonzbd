package deobfuscate

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/par2"
)

// Par2Rename scans dir for .par2 files, builds a mapping of 16KB MD5 hashes
// to original filenames, and renames any obfuscated files that match.
// It opens root for dir once and confines all filesystem operations.
func Par2Rename(ctx context.Context, log *slog.Logger, root *os.Root, dir string, opts fsutil.SanitizeOptions) ([]Rename, error) {
	if log == nil {
		log = slog.Default().With("component", "deobfuscate")
	}

	d, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open root: %w", err)
	}
	defer d.Close() //nolint:errcheck // read-only close
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("readdir: %w", err)
	}

	par2Files := findPar2Files(entries, dir)
	if len(par2Files) == 0 {
		return nil, nil
	}

	hashToName := buildHashToNameMap(log, par2Files)
	if len(hashToName) == 0 {
		return nil, nil
	}

	var renames []Rename
	for _, e := range entries {
		r, renamed, err := par2RenameFile(log, root, dir, e, hashToName, opts)
		if err != nil {
			return renames, err
		}
		if renamed {
			renames = append(renames, r)
		}
	}

	return renames, nil
}

func findPar2Files(entries []fs.DirEntry, dir string) []string {
	var par2Files []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".par2") {
			par2Files = append(par2Files, filepath.Join(dir, e.Name()))
		}
	}
	return par2Files
}

func buildHashToNameMap(log *slog.Logger, par2Files []string) map[[16]byte]string {
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
	return hashToName
}

func par2RenameFile(
	log *slog.Logger,
	root *os.Root,
	dir string,
	e fs.DirEntry,
	hashToName map[[16]byte]string,
	opts fsutil.SanitizeOptions,
) (Rename, bool, error) {
	if !e.Type().IsRegular() {
		return Rename{}, false, nil
	}
	path := filepath.Join(dir, e.Name())
	ext := strings.ToLower(filepath.Ext(e.Name()))

	// Skip if extension is in the excluded list or if it's a PAR2 file itself.
	if excludedExts[ext] || strings.EqualFold(ext, ".par2") {
		return Rename{}, false, nil
	}

	// Calculate 16KB hash using the canonical par2 implementation.
	hash, err := par2.ComputeHash16kRoot(root, e.Name())
	if err != nil {
		log.Debug("deobfuscate: failed to hash file", "path", path, "err", err)
		return Rename{}, false, nil
	}

	trueName, ok := hashToName[hash]
	if !ok || e.Name() == trueName {
		return Rename{}, false, nil
	}

	// Perform rename; GetUniqueRelPath adds a numeric suffix if the
	// par2-recorded target already exists on disk.
	relDst := fsutil.SanitizeFilename(trueName, opts)
	relNewPath := fsutil.GetUniqueRelPath(root, relDst)

	desiredPath := filepath.Join(dir, relDst)
	newPath := filepath.Join(dir, relNewPath)

	if relNewPath != relDst {
		finalPath, skip := resolveConflictingRename(log, root, e.Name(), relDst, path, e.Name(), desiredPath, newPath, hash)
		if skip {
			return Rename{}, false, nil
		}
		newPath = finalPath
		// Re-evaluate relNewPath based on absolute finalPath.
		relNewPath = filepath.Base(newPath)
	}

	r, err := renameRecorded(log, root, e.Name(), relNewPath, path, newPath, trueName, "deobfuscate: par2-renamed")
	if err != nil {
		return Rename{}, false, fmt.Errorf("rename %s → %s: %w", path, newPath, err)
	}
	return r, true, nil
}

// resolveConflictingRename handles the case where the par2-recorded target
// filename already exists on disk. It compares the obfuscated file against
// the existing target by hash and returns the final destination path to
// rename to, or "" if the obfuscated file should be deleted (identical
// duplicate). The second return value is true when the caller should skip
// this entry (delete or unresolvable conflict).
func resolveConflictingRename(
	log *slog.Logger,
	root *os.Root,
	relSrc string,
	relDst string,
	path string, // path of the obfuscated source file
	name string, // entry.Name() for logging
	desiredPath string, // the par2-recorded target (already exists)
	newPath string, // the unique-suffix target path GetUniqueFilename returned
	hash [16]byte,
) (finalPath string, skip bool) {
	// The par2 target already exists. Check whether it has the same
	// content as the obfuscated file. If so, the obfuscated copy is a
	// redundant duplicate — delete it rather than producing a .1 file.
	identical, cerr := contentEqual(root, relSrc, relDst, hash)
	if cerr != nil {
		log.Debug("deobfuscate: content check failed, keeping both files",
			"obfuscated", name, "existing", filepath.Base(desiredPath), "err", cerr)
		return newPath, false
	}
	if identical {
		if rerr := root.Remove(relSrc); rerr != nil {
			log.Warn("deobfuscate: failed to remove duplicate obfuscated file",
				"path", path, "err", rerr)
		} else {
			log.Info("deobfuscate: deleted duplicate obfuscated file",
				"deleted", name,
				"identical_to", filepath.Base(desiredPath),
			)
		}
		return "", true
	}
	log.Warn("deobfuscate: par2 target already exists with different content — renamed to avoid overwriting",
		"obfuscated", name,
		"par2_name", filepath.Base(desiredPath),
		"renamed_to", filepath.Base(newPath),
	)
	return newPath, false
}
