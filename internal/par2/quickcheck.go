package par2

import (
	"crypto/md5"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Rename records a file relocation performed by QuickCheck.
type Rename struct {
	From string // old flat path (relative to dir)
	To   string // new subdirectory path (relative to dir)
}

// QuickCheck matches flat-downloaded files against par2 file manifests and
// relocates them into the correct subdirectory structure. This must run
// before par2 repair so par2 can find files at their expected relative paths.
//
// It runs three matching passes for each par2 entry that contains a
// subdirectory component:
//
//  1. Basename match: flat file "foo.jpg" matches par2 "Screens/foo.jpg"
//  2. Flattened match: flat file "Screens_foo.jpg" matches par2 "Screens/foo.jpg"
//     because SanitizeFilename replaces "/" with "_" during download
//  3. Hash16k match: for remaining unmatched entries, compute MD5 of the first
//     16KB of each unmatched flat file and match against the par2 Hash16k
//
// Errors during individual renames are logged but don't abort — par2 repair
// will report any still-missing files.
func QuickCheck(dir string, sets []Set, log *slog.Logger) ([]Rename, error) {
	if log == nil {
		log = slog.Default()
	}

	// Collect all FileDesc entries from all par2 sets.
	var manifest []FileDesc
	for _, set := range sets {
		parFile := set.MainFile
		if parFile == "" && len(set.ExtraFiles) > 0 {
			parFile = set.ExtraFiles[0]
		}
		if parFile == "" {
			continue
		}
		descs, err := ParseFileDescriptions(parFile)
		if err != nil {
			log.Warn("quickcheck: failed to parse par2 file",
				"file", filepath.Base(parFile), "err", err)
			continue
		}
		manifest = append(manifest, descs...)
	}

	if len(manifest) == 0 {
		return nil, nil
	}

	// Filter to entries that have a subdirectory component — only those
	// need relocation. Entries like "movie.mkv" are already flat.
	var subdirEntries []FileDesc
	for _, fd := range manifest {
		// Normalize backslashes (par2 files may use either separator).
		normalized := filepath.ToSlash(fd.FileName)
		if strings.Contains(normalized, "/") {
			fd.FileName = normalized
			subdirEntries = append(subdirEntries, fd)
		}
	}

	if len(subdirEntries) == 0 {
		log.Info("quickcheck: no par2 entries with subdirectory paths")
		return nil, nil
	}
	log.Info("quickcheck: found par2 entries with subdirectory paths",
		"count", len(subdirEntries))

	// Scan flat files in the download directory (top-level only).
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("quickcheck: readdir %s: %w", dir, err)
	}
	flatFiles := make(map[string]os.DirEntry) // name → entry
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		flatFiles[de.Name()] = de
	}

	var renames []Rename
	matched := make(map[string]bool)   // flat filenames already consumed
	relocated := make(map[string]bool) // par2 entries already relocated

	// Phase 1: Basename match.
	for _, fd := range subdirEntries {
		basename := filepath.Base(fd.FileName)
		if _, ok := flatFiles[basename]; ok && !matched[basename] {
			if relocateFile(dir, basename, fd, log) {
				renames = append(renames, Rename{From: basename, To: fd.FileName})
				matched[basename] = true
				relocated[fd.FileName] = true
			}
		}
	}

	// Phase 2: Flattened name match.
	// SanitizeFilename replaces "/" with "_", so "Screens/foo.jpg" becomes
	// "Screens_foo.jpg" during download.
	for _, fd := range subdirEntries {
		if relocated[fd.FileName] {
			continue
		}
		flattenedName := strings.ReplaceAll(fd.FileName, "/", "_")
		if _, ok := flatFiles[flattenedName]; ok && !matched[flattenedName] {
			if relocateFile(dir, flattenedName, fd, log) {
				renames = append(renames, Rename{From: flattenedName, To: fd.FileName})
				matched[flattenedName] = true
				relocated[fd.FileName] = true
			}
		}
	}

	// Phase 3: Hash16k match for remaining unmatched entries.
	var unmatchedEntries []FileDesc
	for _, fd := range subdirEntries {
		if !relocated[fd.FileName] {
			unmatchedEntries = append(unmatchedEntries, fd)
		}
	}

	if len(unmatchedEntries) > 0 {
		// Build hash16k → FileDesc index for unmatched par2 entries.
		hashIndex := make(map[[16]byte]FileDesc)
		for _, fd := range unmatchedEntries {
			hashIndex[fd.Hash16k] = fd
		}

		// Compute hash16k for each unmatched flat file and try to match.
		for name, de := range flatFiles {
			if matched[name] {
				continue
			}
			if strings.EqualFold(filepath.Ext(name), ".par2") {
				continue
			}

			hash, err := computeHash16k(filepath.Join(dir, de.Name()))
			if err != nil {
				continue // skip files we can't read
			}

			if fd, ok := hashIndex[hash]; ok {
				// Verify file size matches before relocating.
				info, err := de.Info()
				if err != nil {
					continue
				}
				if fd.FileSize > 0 && uint64(info.Size()) != fd.FileSize { //nolint:gosec // size is non-negative
					continue
				}
				if relocateFile(dir, name, fd, log) {
					renames = append(renames, Rename{From: name, To: fd.FileName})
					matched[name] = true
					delete(hashIndex, hash)
				}
			}
		}
	}

	return renames, nil
}

// relocateFile moves a flat file into the par2-specified subdirectory path.
// Returns true on success.
func relocateFile(dir, flatName string, fd FileDesc, log *slog.Logger) bool {
	if log == nil {
		log = slog.Default()
	}
	src := filepath.Join(dir, flatName)

	// Security: verify the par2 filename resolves within dir.
	// Reject path traversal like "../../../etc/passwd".
	targetPath := filepath.Join(dir, filepath.FromSlash(fd.FileName))
	rel, err := filepath.Rel(dir, targetPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		log.Warn("quickcheck: rejected path traversal in par2 filename",
			"par2name", fd.FileName)
		return false
	}

	// Validate file size if we have par2 info and can stat the file.
	if fd.FileSize > 0 {
		info, err := os.Stat(src)
		if err != nil {
			log.Warn("quickcheck: cannot stat source file",
				"file", flatName, "err", err)
			return false
		}
		if uint64(info.Size()) != fd.FileSize { //nolint:gosec // size is non-negative
			log.Info("quickcheck: size mismatch, skipping",
				"file", flatName,
				"have", info.Size(), "want", fd.FileSize)
			return false
		}
	}

	// Create subdirectory.
	destDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		log.Warn("quickcheck: failed to create directory",
			"dir", destDir, "err", err)
		return false
	}

	// Move the file.
	if err := os.Rename(src, targetPath); err != nil {
		log.Warn("quickcheck: failed to rename file",
			"from", flatName, "to", fd.FileName, "err", err)
		return false
	}

	log.Info("quickcheck: relocated file",
		"from", flatName, "to", fd.FileName)
	return true
}

// computeHash16k computes the MD5 hash of the first 16KB of a file,
// matching the Hash16k field in par2 File Description packets.
func computeHash16k(path string) ([16]byte, error) {
	var zero [16]byte

	f, err := os.Open(path) //nolint:gosec // path is constructed from trusted readdir
	if err != nil {
		return zero, err
	}
	defer f.Close() //nolint:errcheck // read-only

	buf := make([]byte, 16*1024)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return zero, err
	}

	return md5.Sum(buf[:n]), nil //nolint:gosec // MD5 used for par2 compatibility, not security
}
