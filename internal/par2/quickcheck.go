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

	"github.com/hobeone/gonzbd/internal/fsutil"
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
// Matching itself lives in Identify, which is pure; this function is the
// part that acts on the answer. It relocates only where the identified file
// is not already at the path par2 records, so it is a no-op for the ordinary
// flat job rather than a self-move for every file in it.
//
// It no longer restricts itself to par2 entries containing a subdirectory
// component. That restriction used to be what kept the old matchers safe —
// they relocated as an inseparable part of matching — and its cost was that
// an obfuscated flat release was never matched at all, so its recovery
// volumes were discarded on the strength of a comparison that could not have
// succeeded (#492).
//
// Errors during individual renames are logged but don't abort — par2 repair
// will report any still-missing files.
func QuickCheck(dir string, sets []Set, log *slog.Logger, opts ...ParseOptions) ([]Rename, error) {
	parseOpts := DefaultParseOptions()
	if len(opts) > 0 {
		parseOpts = opts[0]
	}
	return QuickCheckWithOptions(dir, sets, log, parseOpts)
}

// QuickCheckWithOptions performs QuickCheck matching using caller-specified ParseOptions.
func QuickCheckWithOptions(dir string, sets []Set, log *slog.Logger, opts ParseOptions) ([]Rename, error) {
	if log == nil {
		log = slog.Default()
	}

	id, err := IdentifyWithOptions(dir, sets, log, opts)
	if err != nil {
		return nil, err
	}

	renames := make([]Rename, 0, len(id.Files))
	for _, f := range id.Files {
		// The guard that lets identification run for FLAT sets. par2 names a
		// correctly-placed flat file exactly what it is already called, so
		// relocating every identification would self-move every file in an
		// ordinary job. This used to be enforced by refusing to identify flat
		// sets at all, which is why obfuscated releases went unmatched.
		if !f.NeedsRename() {
			continue
		}
		if relocateFile(dir, f.OnDisk, f.Desc, log) {
			renames = append(renames, Rename{From: f.OnDisk, To: f.Desc.FileName})
		}
	}

	log.Info("quickcheck: complete",
		"identified", len(id.Files),
		"renamed", len(renames),
		"unaccounted", len(id.Unaccounted))

	return renames, nil
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

func relocateFile(dir, flatName string, fd FileDesc, log *slog.Logger) bool {
	if log == nil {
		log = slog.Default()
	}
	src := filepath.Join(dir, flatName)

	// Security: verify the par2 filename resolves within dir.
	// Reject path traversal like "../../../etc/passwd".
	//
	// fsutil.PathWithin rather than a filepath.Rel + HasPrefix("..") pair.
	// That form rejects any name merely BEGINNING with two dots — Rel(dir,
	// dir/"..custom.txt") is "..custom.txt" — so a legitimately-named file
	// was refused relocation and then reported unaccounted. PathWithin tests
	// for ".." as a whole path element, which is the actual escape.
	targetPath := filepath.Join(dir, filepath.FromSlash(fd.FileName))
	if !fsutil.PathWithin(dir, targetPath) {
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
