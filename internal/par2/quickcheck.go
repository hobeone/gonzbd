package par2

import (
	"crypto/md5"
	"fmt"
	"hash/crc32"
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
// It runs four matching passes for each par2 entry that contains a
// subdirectory component:
//
//  1. Basename match: flat file "foo.jpg" matches par2 "Screens/foo.jpg"
//  2. Flattened match: flat file "Screens_foo.jpg" matches par2 "Screens/foo.jpg"
//     because SanitizeFilename replaces "/" with "_" during download
//  3. Hash16k match: for remaining unmatched entries, compute MD5 of the first
//     16KB of each unmatched flat file and match against the par2 Hash16k
//  4. CRC32+Size fallback: compute the full file CRC32 and match against the
//     par2 (FileCRC32, FileSize) tuple — handles obfuscated names (spec §5.3)
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
			log.Info("quickcheck: skipping par2 set with no main file",
				"set", set.Name)
			continue
		}
		log.Info("quickcheck: parsing par2 manifest",
			"file", filepath.Base(parFile))
		descs, err := ParseFileDescriptions(parFile)
		if err != nil {
			log.Warn("quickcheck: failed to parse par2 file",
				"file", filepath.Base(parFile), "err", err)
			continue
		}
		log.Info("quickcheck: par2 manifest entries",
			"file", filepath.Base(parFile), "entries", len(descs))
		manifest = append(manifest, descs...)
	}

	if len(manifest) == 0 {
		log.Info("quickcheck: no file descriptions found in any par2 set")
		return nil, nil
	}

	// Log all manifest entries so we can see exactly what par2 expects.
	log.Info("quickcheck: total par2 manifest entries", "count", len(manifest))
	for i, fd := range manifest {
		log.Info("quickcheck: manifest entry",
			"idx", i,
			"filename", fd.FileName,
			"size", fd.FileSize,
			"hash16k", fmt.Sprintf("%x", fd.Hash16k))
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
		log.Info("quickcheck: no par2 entries contain subdirectory paths — all filenames are flat")
		return nil, nil
	}
	log.Info("quickcheck: par2 entries with subdirectory paths",
		"count", len(subdirEntries))
	for _, fd := range subdirEntries {
		log.Info("quickcheck: needs relocation",
			"par2path", fd.FileName, "size", fd.FileSize)
	}

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

	log.Info("quickcheck: flat files in download dir", "count", len(flatFiles))
	for name := range flatFiles {
		log.Debug("quickcheck: flat file", "name", name)
	}

	var renames []Rename
	matched := make(map[string]bool)   // flat filenames already consumed
	relocated := make(map[string]bool) // par2 entries already relocated

	// Phase 1: Basename match.
	log.Info("quickcheck: Phase 1 — basename matching")
	for _, fd := range subdirEntries {
		basename := filepath.Base(fd.FileName)
		if _, ok := flatFiles[basename]; ok && !matched[basename] {
			log.Info("quickcheck: phase 1 candidate",
				"flat", basename, "par2path", fd.FileName)
			if relocateFile(dir, basename, fd, log) {
				renames = append(renames, Rename{From: basename, To: fd.FileName})
				matched[basename] = true
				relocated[fd.FileName] = true
			}
		} else {
			log.Debug("quickcheck: phase 1 no match",
				"basename", basename, "par2path", fd.FileName,
				"exists", flatFiles[basename] != nil, "already_matched", matched[basename])
		}
	}

	// Phase 2: Flattened name match.
	// SanitizeFilename replaces "/" with "_", so "Screens/foo.jpg" becomes
	// "Screens_foo.jpg" during download.
	log.Info("quickcheck: Phase 2 — flattened name matching (/ → _)")
	for _, fd := range subdirEntries {
		if relocated[fd.FileName] {
			continue
		}
		flattenedName := strings.ReplaceAll(fd.FileName, "/", "_")
		if _, ok := flatFiles[flattenedName]; ok && !matched[flattenedName] {
			log.Info("quickcheck: phase 2 candidate",
				"flat", flattenedName, "par2path", fd.FileName)
			if relocateFile(dir, flattenedName, fd, log) {
				renames = append(renames, Rename{From: flattenedName, To: fd.FileName})
				matched[flattenedName] = true
				relocated[fd.FileName] = true
			}
		} else {
			log.Debug("quickcheck: phase 2 no match",
				"flattenedName", flattenedName, "par2path", fd.FileName,
				"exists", flatFiles[flattenedName] != nil, "already_matched", matched[flattenedName])
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
		log.Info("quickcheck: Phase 3 — hash16k matching",
			"unmatched_entries", len(unmatchedEntries))

		// Build hash16k → FileDesc index for unmatched par2 entries.
		hashIndex := make(map[[16]byte]FileDesc)
		for _, fd := range unmatchedEntries {
			hashIndex[fd.Hash16k] = fd
			log.Info("quickcheck: phase 3 seeking hash16k match",
				"par2path", fd.FileName,
				"hash16k", fmt.Sprintf("%x", fd.Hash16k))
		}

		// Compute hash16k for each unmatched flat file and try to match.
		for name, de := range flatFiles {
			if matched[name] {
				continue
			}
			// Skip verification and metadata files — they're not data files
			// that need relocation.
			ext := strings.ToLower(filepath.Ext(name))
			if ext == ".par2" || ext == ".sfv" || ext == ".nfo" {
				continue
			}

			hash, err := ComputeHash16k(filepath.Join(dir, de.Name()))
			if err != nil {
				log.Debug("quickcheck: phase 3 cannot hash file",
					"name", name, "err", err)
				continue
			}

			if fd, ok := hashIndex[hash]; ok {
				// Verify file size matches before relocating.
				info, err := de.Info()
				if err != nil {
					continue
				}
				if fd.FileSize > 0 && uint64(info.Size()) != fd.FileSize { //nolint:gosec // size is non-negative
					log.Info("quickcheck: phase 3 hash16k matched but size differs",
						"flat", name, "par2path", fd.FileName,
						"flatSize", info.Size(), "par2Size", fd.FileSize)
					continue
				}
				log.Info("quickcheck: phase 3 hash16k match found",
					"flat", name, "par2path", fd.FileName)
				if relocateFile(dir, name, fd, log) {
					renames = append(renames, Rename{From: name, To: fd.FileName})
					matched[name] = true
					delete(hashIndex, hash)
				}
			}
		}

		// Log any entries that remained unmatched after phase 3.
		for _, fd := range hashIndex {
			log.Debug("quickcheck: phase 3 unmatched, will try CRC fallback",
				"par2path", fd.FileName, "size", fd.FileSize)
		}
	} else {
		log.Info("quickcheck: Phase 3 — skipped, all subdir entries matched")
	}

	// Phase 4: CRC32+FileSize fallback (spec §5.3).
	// For entries still unmatched after hash16k, try matching by the full
	// file CRC32 and file size tuple. This is the most expensive pass
	// (requires reading the entire file), but handles cases where:
	//   - hash16k is ambiguous (HasDuplicate excludes both entries)
	//   - the file was renamed by the poster
	var unmatchedPhase4 []FileDesc
	for _, fd := range subdirEntries {
		if !relocated[fd.FileName] && fd.FileCRC32 > 0 && fd.FileSize > 0 {
			unmatchedPhase4 = append(unmatchedPhase4, fd)
		}
	}

	if len(unmatchedPhase4) > 0 {
		log.Info("quickcheck: Phase 4 — CRC32+size fallback matching",
			"unmatched_entries", len(unmatchedPhase4))

		// Build (crc32, filesize) → FileDesc index.
		type crcSizeKey struct {
			crc  uint32
			size uint64
		}
		crcIndex := make(map[crcSizeKey]FileDesc)
		for _, fd := range unmatchedPhase4 {
			crcIndex[crcSizeKey{crc: fd.FileCRC32, size: fd.FileSize}] = fd
		}

		// Compute CRC32 for each unmatched flat file.
		for name, de := range flatFiles {
			if matched[name] || len(crcIndex) == 0 {
				continue
			}
			ext := strings.ToLower(filepath.Ext(name))
			if ext == ".par2" || ext == ".sfv" || ext == ".nfo" {
				continue
			}

			info, err := de.Info()
			if err != nil {
				continue
			}
			fileSize := uint64(info.Size()) //nolint:gosec // size is non-negative

			// Quick pre-check: does any unmatched entry have this file size?
			sizeMatch := false
			for k := range crcIndex {
				if k.size == fileSize {
					sizeMatch = true
					break
				}
			}
			if !sizeMatch {
				continue
			}

			fileCRC, err := computeFileCRC32(filepath.Join(dir, name))
			if err != nil {
				log.Debug("quickcheck: phase 4 cannot compute CRC32",
					"name", name, "err", err)
				continue
			}

			key := crcSizeKey{crc: fileCRC, size: fileSize}
			if fd, ok := crcIndex[key]; ok {
				log.Info("quickcheck: phase 4 CRC32+size match found",
					"flat", name, "par2path", fd.FileName,
					"crc32", fmt.Sprintf("%08x", fileCRC), "size", fileSize)
				if relocateFile(dir, name, fd, log) {
					renames = append(renames, Rename{From: name, To: fd.FileName})
					matched[name] = true
					relocated[fd.FileName] = true
					delete(crcIndex, key)
				}
			}
		}

		// Log any entries that remained unmatched after all four phases.
		for _, fd := range crcIndex {
			log.Warn("quickcheck: par2 entry unmatched after all phases",
				"par2path", fd.FileName, "size", fd.FileSize)
		}
	}

	log.Info("quickcheck: complete",
		"total_renames", len(renames),
		"total_subdir_entries", len(subdirEntries))

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

// ComputeHash16k computes the MD5 hash of the first 16KB of a file,
// matching the Hash16k field in par2 File Description packets.
// This is exported for use by the deobfuscate package.
func ComputeHash16k(path string) ([16]byte, error) {
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

// computeFileCRC32 computes the CRC32 (IEEE) of the entire file at path.
// Used by Phase 4 of QuickCheck for the (CRC32, FileSize) fallback match.
func computeFileCRC32(path string) (uint32, error) {
	f, err := os.Open(path) //nolint:gosec // path is constructed from trusted readdir
	if err != nil {
		return 0, err
	}
	defer f.Close() //nolint:errcheck // read-only

	h := crc32.NewIEEE()
	if _, err := io.Copy(h, f); err != nil {
		return 0, err
	}
	return h.Sum32(), nil
}
