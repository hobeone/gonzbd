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

// RAR3 block header types (rarengine has no exported constants for these --
// they're internal to its own engine_rar3.go, which uses the same raw
// values).
const (
	rar3HeaderTypeFile       = 0x74
	rar3HeaderTypeTerminator = 0x7b
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

	if ver == 3 {
		info, err := InspectRar3(p)
		if err == nil {
			return info, nil
		}
		// Same fallback rationale as RAR5 above: encrypted headers (or any
		// other parse failure) surface as an error here, so fall back to
		// unrar vt to be absolutely sure.
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
		defer func() { _ = f.Close() }() // cleanup error in defer

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

// InspectRar3 opens the file at path and inspects it as a RAR3/RAR4 archive
// using the pure Go rarengine library. It reads only block headers -- each
// file's packed (and possibly compressed) payload is skipped directly on
// the raw stream via its header-reported PackedSize, never decompressed.
//
// This deliberately does not use rarengine.StreamDecompressor (the
// InspectRar5 approach): that walk skips to the next file by decompressing
// and discarding the previous file's content, and rarengine's RAR3 engine
// only implements the store compression method (method 0) -- any real
// compressed RAR3 archive would fail there. Reading PackedSize directly off
// the header and skipping raw bytes works regardless of compression
// method, since header inspection never needs the decompressed content.
// See https://github.com/hobeone/rarengine/issues/14 for the RAR3
// decompression gap.
func InspectRar3(p string) (info Info, err error) {
	info.Version = 3
	err = cmdutil.SafeEngineRun("rarheader: rarengine panic", func() error {
		f, err := openPastSignature(p, rar3Sig)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }() // cleanup error in defer

		for {
			h, err := rarengine.ReadRAR3BlockHeader(f)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return fmt.Errorf("rarheader: read block header: %w", err)
			}

			switch h.Type {
			case rar3HeaderTypeFile:
				fh, err := rarengine.ParseRAR3FileHeader(h)
				if err != nil {
					return fmt.Errorf("rarheader: parse file header: %w", err)
				}
				if !fh.IsDir {
					info.Filenames = append(info.Filenames, sanitizeName(fh.Name))
				}
				if fh.Encrypted {
					info.Encrypted = true
				}
				// fh.PackedSize's high 32 bits come from an attacker-controlled
				// header field (the RAR3 "large file" HIGH_PACK extension) OR'd
				// into an int64 -- a crafted value with the sign bit set makes
				// PackedSize negative. io.CopyN treats N<=0 as "nothing to do"
				// and returns (0, nil) rather than an error, which would leave
				// the stream desynced (positioned inside this file's packed
				// data instead of past it) and let a crafted archive splice in
				// attacker-chosen bytes as the next header. Reject explicitly
				// instead of relying on the next header's CRC check to
				// eventually catch the desync.
				if fh.PackedSize < 0 {
					return fmt.Errorf("rarheader: file %q: negative packed size %d", fh.Name, fh.PackedSize)
				}
				if err := skipForward(f, fh.PackedSize); err != nil {
					return fmt.Errorf("rarheader: skip packed data: %w", err)
				}

			case rar3HeaderTypeTerminator:
				return nil

			default:
				if err := skipForward(f, h.DataSize); err != nil {
					return fmt.Errorf("rarheader: skip block data: %w", err)
				}
			}
		}
	})
	return info, err
}

// VerifyPassword opens the RAR5 archive at mainFilePath and checks whether
// password matches its embedded password check value, without extracting
// or decompressing any file content. It inspects, in order:
//
//   - If the archive's own headers are encrypted (HEAD_CRYPT), it verifies
//     against the archive-wide check value.
//   - Otherwise, if the first file entry is content-encrypted, it verifies
//     against that file's per-file check value.
//   - If neither is present -- the archive isn't encrypted at all, or an
//     older archive carries no check value -- hasCheckValue is false and
//     verified is false: there is nothing to definitively check, and
//     callers must fall back to a real extraction attempt.
//
// Only RAR5 is supported (verified=false, hasCheckValue=false, err=nil for
// RAR3/4 archives) -- RAR3's SHA1-based check is out of scope for now.
func VerifyPassword(mainFilePath, password string) (verified, hasCheckValue bool, err error) {
	ver, err := readMagic(mainFilePath)
	if err != nil {
		return false, false, err
	}
	if ver != 5 {
		return false, false, nil
	}

	f, err := openPastSignature(mainFilePath, rar5Sig)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = f.Close() }() // cleanup error in defer

	return verifyPasswordFromHeaders(f, password)
}

// verifyPasswordFromHeaders scans RAR5 block headers from r (positioned
// just past the magic signature) for either an archive-level HEAD_CRYPT
// header or the first file entry, returning the corresponding check-value
// verification result. r is a plain io.Reader (no seeking) so block payload
// data that isn't needed is skipped via DataSize.
func verifyPasswordFromHeaders(r io.Reader, password string) (verified, hasCheckValue bool, err error) {
	for {
		h, err := rarengine.ReadBlockHeader(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, false, nil
			}
			return false, false, fmt.Errorf("rarheader: read block header: %w", err)
		}

		switch h.Type {
		case rarengine.HeaderTypeEncryption:
			ch, err := rarengine.ParseCryptHeader(h)
			if err != nil {
				return false, false, fmt.Errorf("rarheader: parse crypt header: %w", err)
			}
			return rarengine.VerifyPassword(ch, password)

		case rarengine.HeaderTypeFile, rarengine.HeaderTypeService:
			fh, err := rarengine.ParseFileHeader(h)
			if err != nil {
				return false, false, fmt.Errorf("rarheader: parse file header: %w", err)
			}
			if !fh.Encrypted {
				return false, false, nil
			}
			return rarengine.VerifyFilePassword(fh, password)

		case rarengine.HeaderTypeEnd:
			return false, false, nil

		default:
			if h.DataSize > 0 {
				if _, err := io.CopyN(io.Discard, r, h.DataSize); err != nil {
					return false, false, fmt.Errorf("rarheader: skip block data: %w", err)
				}
			}
		}
	}
}

// RecoverVolumeExtension opens the RAR5 archive at path and reads its main
// archive header to recover volume sequencing information, without
// decompressing any file content. It is used to reconstruct a canonical
// filename (e.g. "name.part003.rar") for a RAR volume whose on-disk name
// carries no numbering clue at all.
//
// volumeIndex is 0-indexed: the first volume in a set has no explicit
// volume-number flag in the RAR5 format and is normalized to 0 here; the
// second volume is 1, the third is 2, and so on. multiVolume is false for a
// single, non-split archive, in which case volumeIndex is always 0.
//
// Only RAR5 is supported -- RAR3 has no equivalent field in its main
// archive header (the volume number lives in the RAR3 end-of-archive block,
// which this package does not parse). Returns ErrNotRAR if path is not a
// valid RAR archive at all, and a non-nil error (not ErrNotRAR) for a
// recognized RAR3 archive, since volume recovery cannot be performed for it.
func RecoverVolumeExtension(p string) (volumeIndex int, multiVolume bool, err error) {
	ver, err := readMagic(p)
	if err != nil {
		return 0, false, err
	}
	if ver != 5 {
		return 0, false, fmt.Errorf("rarheader: RAR%d volume recovery not supported (RAR5 only)", ver)
	}

	f, err := openPastSignature(p, rar5Sig)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = f.Close() }() // cleanup error in defer

	h, err := rarengine.ReadBlockHeader(f)
	if err != nil {
		return 0, false, fmt.Errorf("rarheader: read block header: %w", err)
	}
	if h.Type != rarengine.HeaderTypeArchive {
		return 0, false, fmt.Errorf("rarheader: expected archive header block, got type %d", h.Type)
	}

	ah, err := rarengine.ParseArchiveHeader(h)
	if err != nil {
		return 0, false, fmt.Errorf("rarheader: parse archive header: %w", err)
	}

	if !ah.MultiVolume {
		return 0, false, nil
	}
	if ah.VolumeNumber < 0 {
		return 0, true, nil // first volume: RAR5 omits the explicit flag
	}
	return ah.VolumeNumber, true, nil
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

// skipForward advances f by n bytes using Seek rather than reading and
// discarding n bytes -- this matters because header inspection routinely
// skips whole (possibly multi-hundred-MB to multi-GB) file payloads it will
// never read. Seek alone is not sufficient: os.File.Seek past the end of a
// file succeeds silently (the error only surfaces on a later Read), which
// would turn a truncated/corrupt archive into a false "clean" inspection
// instead of the explicit error a corrupt archive should produce. Stat
// confirms the new position is still within the file before continuing.
func skipForward(f *os.File, n int64) error {
	if n <= 0 {
		return nil
	}
	newPos, err := f.Seek(n, io.SeekCurrent)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if newPos > fi.Size() {
		return fmt.Errorf("seek past end of file (pos=%d, size=%d)", newPos, fi.Size())
	}
	return nil
}

// osOpen is a seam for tests to inject an open failure independent of
// readMagic's own os.Open call, since callers always readMagic(p)
// successfully before calling openPastSignature(p, ...) on the same
// unchanged path -- without this seam, openPastSignature's open-error
// branch would be unreachable through any real caller.
//
// Tradeoff: gosec's G304 (tainted-path-open) check pattern-matches literal
// os.Open/os.OpenFile call expressions, so routing the call through this
// func-var indirection means gosec no longer flags it at all -- not even
// as something to suppress. The //nolint:gosec below is now decorative
// (verified: removing it produces the same 0 lint issues), not an active
// suppression. This is an accepted, bounded tradeoff, not an oversight:
// every os.Open(p) callsite in this file already carries an identical
// "p is trusted input from internal caller" //nolint:gosec justification
// (readMagic, inspectViaUnrar's use of p) -- the same trust boundary this
// package already relies on elsewhere, just no longer independently
// re-verified by the linter at this specific callsite.
var osOpen = os.Open

// openPastSignature opens p and advances past its len(sig)-byte magic
// signature, returning the file positioned at the first header byte. The
// caller owns the returned file and must close it.
func openPastSignature(p string, sig []byte) (*os.File, error) {
	//nolint:gosec // p is trusted input from internal caller
	f, err := osOpen(p)
	if err != nil {
		return nil, fmt.Errorf("rarheader: open %s: %w", p, err)
	}
	if _, err := io.CopyN(io.Discard, f, int64(len(sig))); err != nil {
		_ = f.Close() // close on the error path; no defer here since f is returned on success
		return nil, fmt.Errorf("rarheader: skip signature: %w", err)
	}
	return f, nil
}

// readMagic opens p, reads up to 8 bytes, and returns the RAR version
// (3 or 5) based on the magic signature. Returns ErrNotRAR if the file
// does not start with a valid RAR signature.
func readMagic(p string) (int, error) {
	f, err := os.Open(p) //nolint:gosec // p from trusted internal callers
	if err != nil {
		return 0, fmt.Errorf("rarheader: open %s: %w", p, err)
	}
	defer func() { _ = f.Close() }() // cleanup error in defer

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
