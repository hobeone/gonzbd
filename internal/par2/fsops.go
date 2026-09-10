package par2

import (
	"crypto/md5" //nolint:gosec // MD5 is mandated by the PAR2 format, not security-sensitive
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// Rename records a file relocation ApplyRenames performed.
type Rename struct {
	From string // old flat path (relative to dir)
	To   string // new subdirectory path (relative to dir)
}

func collectManifests(sets []Set, log *slog.Logger) []FileDesc {
	return collectManifestsWithOptions(sets, log, DefaultParseOptions())
}

func collectManifestsWithOptions(sets []Set, log *slog.Logger, opts ParseOptions) []FileDesc {
	if log == nil {
		log = slog.Default()
	}
	var manifest []FileDesc
	for _, set := range sets {
		parFile := set.ParseFile()
		if parFile == "" {
			log.Info("quickcheck: skipping par2 set with no main file",
				"set", set.Name)
			continue
		}
		log.Info("quickcheck: parsing par2 manifest",
			"file", filepath.Base(parFile))
		descs, err := ParseFileDescriptionsWithOptions(parFile, opts)
		if err != nil {
			log.Warn("quickcheck: failed to parse par2 file",
				"file", filepath.Base(parFile), "err", err)
			continue
		}
		log.Info("quickcheck: par2 manifest entries",
			"file", filepath.Base(parFile), "entries", len(descs))
		manifest = append(manifest, descs...)
	}
	return manifest
}

func scanFlatFiles(dir string, log *slog.Logger) (map[string]os.DirEntry, error) {
	if log == nil {
		log = slog.Default()
	}
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
	return flatFiles, nil
}

// relocateFile moves flatName to the path fd records, confined to root.
//
// fd.FileName is poster-controlled — it comes out of parseFileDescBody with
// only encoding correction applied — and it becomes a filesystem path here, so
// Standing Design Rule 1's carve-out keeps a guard on it regardless of what
// wrote the set.
//
// That guard is os.Root, and it replaces the lexical fsutil.PathWithin check
// this used to carry. Its contract refuses any name whose components reference
// a location outside the root: "..", an absolute path, and a symlinked
// component pointing outward are all rejected by the Root methods themselves,
// so there is no separate check to forget at a new call site. The lexical form
// could only ever inspect the name; this inspects the resolved location, which
// is where the property actually lives.
//
// It still accepts a name merely BEGINNING with two dots — "..config.txt" is an
// ordinary component, not a traversal — which the filepath.Rel + HasPrefix pair
// that preceded PathWithin got wrong, refusing the file and then reporting it
// unaccounted.
func relocateFile(root *os.Root, flatName string, fd FileDesc, log *slog.Logger) bool {
	if log == nil {
		log = slog.Default()
	}
	destRel := filepath.FromSlash(fd.FileName)

	// Validate file size if we have par2 info and can stat the file.
	if fd.FileSize > 0 {
		info, err := root.Stat(flatName)
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

	// Create subdirectory. Skipped for a flat name, where Dir is "." and the
	// root already exists.
	if destDir := filepath.Dir(destRel); destDir != "." {
		if err := root.MkdirAll(destDir, 0o750); err != nil {
			log.Warn("quickcheck: failed to create directory",
				"dir", destDir, "err", err)
			return false
		}
	}

	// Move the file.
	if err := root.Rename(flatName, destRel); err != nil {
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

	return hash16kOfReader(f)
}

// ComputeHash16kRoot computes the MD5 hash of the first 16KB of a file,
// relative to an os.Root handle.
func ComputeHash16kRoot(root *os.Root, relPath string) ([16]byte, error) {
	var zero [16]byte

	f, err := root.Open(relPath)
	if err != nil {
		return zero, err
	}
	defer f.Close() //nolint:errcheck // read-only

	return hash16kOfReader(f)
}

// hash16kOf is the single owner of what "the Hash16k of this content" means,
// so the two exported wrappers above cannot disagree about it. They differ
// only in how they open a file.
//
// Both short reads are successes, and they are different errors. io.ReadFull
// returns io.ErrUnexpectedEOF when it read SOME of the 16 KB and io.EOF when
// it read NONE — so treating only the former as success made a 0-byte file
// fail to hash at all. Identify then logged "could not hash candidate" and
// skipped it, leaving a par2 entry for an empty file permanently unaccounted
// and fetching recovery volumes over it. par2 records the MD5 of the first
// 16 KB, or of the whole file where it is smaller, and md5.Sum(nil) is the
// correct answer for an empty one.
func hash16kOfReader(r io.Reader) ([16]byte, error) {
	var zero [16]byte

	buf := make([]byte, 16*1024)
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
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

type crcSizeKey struct {
	crc  uint32
	size uint64
}
