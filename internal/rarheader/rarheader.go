// Package rarheader provides read-only RAR archive header inspection.
//
// It inspects RAR3 and RAR5 archives to extract file metadata
// (internal filenames, encryption status, multi-volume flags)
// without performing any decompression.
//
// Primary use case: deobfuscation of Usenet downloads where outer filenames
// are randomized but the RAR headers contain the original content names.
package rarheader

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/hobeone/rarengine"
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
func IsRAR(p string) (bool, error) {
	ver, err := readMagic(p)
	if errors.Is(err, ErrNotRAR) {
		return false, nil
	}
	return ver > 0, err
}

// Inspect opens the file at path and reads its RAR headers without
// decompressing any content. It returns an Info struct describing the
// archive's contents and properties.
//
// Returns ErrNotRAR if the file is not a valid RAR archive.
// For encrypted-header archives, returns partial Info with HeaderEncrypted=true.
func Inspect(p string) (Info, error) {
	ver, err := readMagic(p)
	if err != nil {
		return Info{}, err
	}

	if ver == 5 {
		info, err := InspectRar5(p)
		if err == nil {
			return info, nil
		}
		// If it's encrypted header, rarengine will fail with bad header CRC.
		// Fallback to unrar vt to be absolutely sure.
	}

	// Fallback to unrar vt for RAR3/4 or if rarengine failed
	return inspectViaUnrar(p, ver)
}

func InspectRar5(p string) (info Info, err error) {
	info.Version = 5

	f, err := os.Open(p)
	if err != nil {
		return info, err
	}
	defer f.Close()

	volumesChan := make(chan io.ReadCloser, 1)
	volumesChan <- f
	close(volumesChan)

	sd := rarengine.NewStreamDecompressor(volumesChan)

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("rarheader: rarengine panic: %v", r)
		}
	}()

	for {
		fh, err := sd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, rarengine.ErrNoNextVolume) {
				break
			}
			return info, err
		}
		if !fh.IsDir {
			info.Filenames = append(info.Filenames, sanitizeName(fh.Name))
		}
		if fh.Encrypted {
			info.Encrypted = true
		}
	}
	return info, nil
}

func inspectViaUnrar(p string, ver int) (Info, error) {
	var info Info
	info.Version = ver

	cmd := exec.Command("unrar", "vt", "-p-", p)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	output := stdout.String()
	stderrStr := stderr.String()

	if err != nil {
		if strings.Contains(output, "Incorrect password") || strings.Contains(stderrStr, "Incorrect password") ||
			strings.Contains(output, "password") || strings.Contains(stderrStr, "password") {
			info.HeaderEncrypted = true
			info.Encrypted = true
			return info, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 11 {
			info.HeaderEncrypted = true
			info.Encrypted = true
			return info, nil
		}
		return info, fmt.Errorf("rarheader: unrar vt failed: %w (stderr: %q)", err, stderrStr)
	}

	lines := strings.Split(output, "\n")
	var currentName string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "Name:"); ok {
			currentName = strings.TrimSpace(after)
			info.Filenames = append(info.Filenames, sanitizeName(currentName))
		} else if after, ok := strings.CutPrefix(line, "Flags:"); ok {
			flags := strings.TrimSpace(after)
			if strings.Contains(flags, "encrypted") {
				info.Encrypted = true
			}
		}
	}

	return info, nil
}

// readMagic opens path, reads up to 8 bytes, and returns the RAR version
// (3 or 5) based on the magic signature. Returns ErrNotRAR if the file
// does not start with a valid RAR signature.
func readMagic(path string) (int, error) {
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

	if n >= len(rar5Sig) && bytes.Equal(buf[:len(rar5Sig)], rar5Sig) {
		return 5, nil
	}
	if n >= len(rar3Sig) && bytes.Equal(buf[:len(rar3Sig)], rar3Sig) {
		return 3, nil
	}

	return 0, ErrNotRAR
}

// sanitizeName strips directory traversal components from an untrusted
// RAR header filename.
func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.ReplaceAll(name, "\x00", "_")
	name = path.Base(name)
	if name == "" || name == "." || name == "/" {
		return "unknown"
	}
	return name
}
