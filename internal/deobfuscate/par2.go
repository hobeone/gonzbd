package deobfuscate

import (
	"crypto/md5" //nolint:gosec // md5 is used for identification by PAR2 spec
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hobeone/sabnzbd-go/internal/par2"
)

// Par2Rename scans dir for .par2 files, builds a mapping of 16KB MD5 hashes
// to original filenames, and renames any obfuscated files that match.
func Par2Rename(dir string) ([]Rename, error) {
	log := slog.Default().With("component", "deobfuscate")

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

		// Calculate 16KB hash.
		hash, err := hash16k(path)
		if err != nil {
			log.Debug("deobfuscate: failed to hash file", "path", path, "err", err)
			continue
		}

		// Check if we have a match.
		if trueName, ok := hashToName[hash]; ok {
			if e.Name() == trueName {
				continue
			}

			// Perform rename.
			newPath, err := uniqueName(filepath.Join(dir, trueName))
			if err != nil {
				return renames, err
			}

			if err := os.Rename(path, newPath); err != nil {
				return renames, fmt.Errorf("rename %s → %s: %w", path, newPath, err)
			}
			log.Info("deobfuscate: par2-renamed", "from", path, "to", newPath)
			renames = append(renames, Rename{From: path, To: newPath})
		}
	}

	return renames, nil
}

// hash16k returns the MD5 hash of the first 16,384 bytes of a file.
func hash16k(path string) ([16]byte, error) {
	var result [16]byte
	f, err := os.Open(path) //nolint:gosec // path is constructed from trusted readdir
	if err != nil {
		return result, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	h := md5.New() //nolint:gosec // md5 is used for identification by PAR2 spec
	_, err = io.CopyN(h, f, 16384)
	if err != nil && !errors.Is(err, io.EOF) {
		return result, err
	}

	copy(result[:], h.Sum(nil))
	return result, nil
}
