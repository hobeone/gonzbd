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

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

var (
	// rar3Sig is the RAR3 magic signature (7 bytes).
	rar3Sig = []byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07, 0x00}

	// rar5Sig is the RAR5 magic signature (8 bytes).
	rar5Sig = []byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07, 0x01, 0x00}
)

// ErrNotRAR is returned when a file does not start with a valid RAR signature.
var ErrNotRAR = errors.New("rarheader: not a RAR archive")

var execCommand = exec.Command

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

// IsRARReader checks whether the reader starts with a RAR3 or RAR5 magic
// signature.
func IsRARReader(r io.Reader) (bool, error) {
	buf := make([]byte, 8)
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	if n >= len(rar5Sig) && bytes.Equal(buf[:len(rar5Sig)], rar5Sig) {
		return true, nil
	}
	if n >= len(rar3Sig) && bytes.Equal(buf[:len(rar3Sig)], rar3Sig) {
		return true, nil
	}
	return false, nil
}

// Version returns the RAR format version (3 or 5) of the file at path,
// based on its magic signature. It reads at most 8 bytes and performs no
// header parsing, so it is suitable for fast pre-filtering before choosing
// an extraction strategy. Returns ErrNotRAR if the file does not start
// with a valid RAR signature.
func Version(p string) (int, error) {
	return readMagic(p)
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

// InspectRar5 opens the file at path and inspects it as a RAR5 archive
// using the pure Go rarengine library.
func InspectRar5(p string) (info Info, err error) {
	info.Version = 5
	err = cmdutil.SafeEngineRun("rarheader: rarengine panic", func() error {
		//nolint:gosec // p is trusted input from internal caller
		f, openErr := os.Open(p)
		if openErr != nil {
			return openErr
		}
		defer func() { _ = f.Close() }() //nolint:errcheck // cleanup error in defer

		volumesChan := make(chan io.ReadCloser, 1)
		volumesChan <- f
		close(volumesChan)

		sd := rarengine.NewStreamDecompressor(volumesChan)

		for {
			fh, nextErr := sd.Next()
			if nextErr != nil {
				if errors.Is(nextErr, io.EOF) || errors.Is(nextErr, rarengine.ErrNoNextVolume) {
					break
				}
				return nextErr
			}
			if !fh.IsDir {
				info.Filenames = append(info.Filenames, sanitizeName(fh.Name))
			}
			if fh.Encrypted {
				info.Encrypted = true
			}
		}
		return nil
	})
	return info, err
}

func inspectViaUnrar(p string, ver int) (Info, error) {
	var info Info
	info.Version = ver

	//nolint:gosec // p is trusted input from internal caller
	cmd := execCommand("unrar", "vt", "-p-", p)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	output := stdout.String()
	stderrStr := stderr.String()

	if err != nil {
		if isPasswordError(err, output, stderrStr) {
			info.HeaderEncrypted = true
			info.Encrypted = true
			return info, nil
		}
		return info, fmt.Errorf("rarheader: unrar vt failed: %w (stderr: %q)", err, stderrStr)
	}

	info.Filenames, info.Encrypted = parseUnrarVtOutput(output)
	return info, nil
}

// isPasswordError checks if the unrar error, stdout, or stderr indicates
// a password-related error (incorrect password or missing password).
func isPasswordError(err error, stdout, stderr string) bool {
	if strings.Contains(stdout, "Incorrect password") || strings.Contains(stderr, "Incorrect password") ||
		strings.Contains(stdout, "password") || strings.Contains(stderr, "password") {
		return true
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 11 {
		return true
	}
	return false
}

// parseUnrarVtOutput parses the stdout of 'unrar vt' to extract filenames
// and encryption status.
func parseUnrarVtOutput(output string) (filenames []string, encrypted bool) {
	lines := strings.Split(output, "\n")
	var currentName string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "Name:"); ok {
			currentName = strings.TrimSpace(after)
			filenames = append(filenames, sanitizeName(currentName))
		} else if after, ok := strings.CutPrefix(line, "Flags:"); ok {
			flags := strings.TrimSpace(after)
			if strings.Contains(flags, "encrypted") {
				encrypted = true
			}
		}
	}
	return filenames, encrypted
}

// readMagic opens p, reads up to 8 bytes, and returns the RAR version
// (3 or 5) based on the magic signature. Returns ErrNotRAR if the file
// does not start with a valid RAR signature.
func readMagic(p string) (int, error) {
	f, err := os.Open(p) //nolint:gosec // p from trusted internal callers
	if err != nil {
		return 0, fmt.Errorf("rarheader: open %s: %w", p, err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // cleanup error in defer

	buf := make([]byte, 8)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("rarheader: read %s: %w", p, err)
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
	if name == "" || name == "." || name == ".." || name == "/" {
		return "unknown"
	}
	return name
}
