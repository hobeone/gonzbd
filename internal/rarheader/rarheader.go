// Package rarheader provides read-only RAR archive header inspection.
//
// It wraps github.com/nwaples/rardecode/v2 to extract file metadata
// (internal filenames, encryption status, multi-volume flags) from RAR3
// and RAR5 archives without performing any decompression.
//
// Primary use case: deobfuscation of Usenet downloads where outer filenames
// are randomized but the RAR headers contain the original content names.
package rarheader

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	rardecode "github.com/nwaples/rardecode/v2"
)

var (
	// rar3Sig is the RAR3 magic signature (7 bytes).
	rar3Sig = []byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07, 0x00}

	// rar5Sig is the RAR5 magic signature (8 bytes).
	rar5Sig = []byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07, 0x01, 0x00}
)

// ErrNotRAR is returned when a file does not start with a valid RAR signature.
var ErrNotRAR = errors.New("rarheader: not a RAR archive")

// Info holds metadata extracted from a RAR archive's headers.
type Info struct {
	// Version is the RAR format version: 3 for RAR3, 5 for RAR5.
	// Zero if detection failed before version could be determined.
	Version int

	// Filenames lists the internal filenames from the archive's file headers.
	// These are the original names of the compressed files, which are often
	// the unobfuscated content names.
	Filenames []string

	// Encrypted is true if any file header indicates encryption.
	Encrypted bool

	// HeaderEncrypted is true if the file headers themselves are encrypted
	// (password required even to list contents).
	HeaderEncrypted bool
}

// IsRAR checks whether the file at path starts with a RAR3 or RAR5 magic
// signature. It reads at most 8 bytes and is suitable for fast pre-filtering.
func IsRAR(path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // path from trusted internal callers
	if err != nil {
		return false, fmt.Errorf("rarheader: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 8)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("rarheader: read %s: %w", path, err)
	}
	if n < len(rar3Sig) {
		return false, nil
	}

	if bytesEqual(buf[:len(rar3Sig)], rar3Sig) {
		return true, nil
	}
	if n >= len(rar5Sig) && bytesEqual(buf[:len(rar5Sig)], rar5Sig) {
		return true, nil
	}
	return false, nil
}

// Inspect opens the file at path and reads its RAR headers without
// decompressing any content. It returns an Info struct describing the
// archive's contents and properties.
//
// Returns ErrNotRAR if the file is not a valid RAR archive.
// For encrypted-header archives, returns partial Info with HeaderEncrypted=true.
func Inspect(path string) (Info, error) {
	var info Info

	// Detect version from magic bytes.
	ver, err := detectVersion(path)
	if err != nil {
		return info, err
	}
	info.Version = ver

	// Use rardecode.List for header-only inspection (no decompression).
	// The library can panic on malformed/truncated archives (e.g. slice
	// bounds out of range in archive50.readBlockHeader), so we wrap the
	// call in a recover to convert panics into errors.
	files, listErr := safeList(path)
	if listErr != nil {
		// rardecode returns ErrNoSig for non-RAR files.
		if errors.Is(listErr, rardecode.ErrNoSig) {
			return info, ErrNotRAR
		}
		// Encrypted headers cause errors but we may have partial data.
		if errors.Is(listErr, rardecode.ErrBadPassword) {
			info.HeaderEncrypted = true
			info.Encrypted = true
			return info, nil
		}
		return info, fmt.Errorf("rarheader: list %s: %w", path, listErr)
	}

	for _, f := range files {
		info.Filenames = append(info.Filenames, sanitizeName(f.Name))
		if f.Encrypted || f.HeaderEncrypted {
			info.Encrypted = true
		}
		if f.HeaderEncrypted {
			info.HeaderEncrypted = true
		}
	}

	return info, nil
}

// detectVersion reads the magic bytes to determine if the archive is RAR3 or RAR5.
func detectVersion(path string) (int, error) {
	f, err := os.Open(path) //nolint:gosec // path from trusted internal callers
	if err != nil {
		return 0, fmt.Errorf("rarheader: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 8)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("rarheader: read %s: %w", path, err)
	}

	if n >= len(rar5Sig) && bytesEqual(buf[:len(rar5Sig)], rar5Sig) {
		return 5, nil
	}
	if n >= len(rar3Sig) && bytesEqual(buf[:len(rar3Sig)], rar3Sig) {
		return 3, nil
	}

	return 0, ErrNotRAR
}

// sanitizeName strips directory traversal components from an untrusted
// RAR header filename. It uses path.Base (forward-slash aware, OS-independent)
// to extract the final path component, replaces null bytes, and falls back
// to "unknown" for empty results.
func sanitizeName(name string) string {
	// Normalize both forward and back slashes to forward slashes.
	// filepath.ToSlash only converts os.PathSeparator, which on Linux
	// is already '/'. RAR archives from Windows use '\' as separator,
	// so we must handle that explicitly.
	name = strings.ReplaceAll(name, "\\", "/")
	// Strip null bytes — these can bypass string comparisons.
	name = strings.ReplaceAll(name, "\x00", "_")
	// path.Base returns the last element of the path.
	name = path.Base(name)
	if name == "" || name == "." || name == "/" {
		return "unknown"
	}
	return name
}

// safeList wraps rardecode.List with a deferred recover. The rardecode
// library can panic on malformed or truncated archives (e.g. slice bounds
// out of range in archive50.readBlockHeader). This function converts such
// panics into a returned error so callers don't crash.
func safeList(path string) (files []*rardecode.File, err error) {
	defer func() {
		if r := recover(); r != nil {
			files = nil
			err = fmt.Errorf("rarheader: rardecode panic on %s: %v", path, r)
		}
	}()
	return rardecode.List(path)
}

// bytesEqual compares two byte slices for equality without importing bytes.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
