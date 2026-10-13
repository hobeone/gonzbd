package unpack

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

// errTarBomb is returned when a tar entry's declared or actually-read size
// exceeds opts.MaxSize/MaxRatio. Plain (uncompressed) tar is not immune to
// decompression-bomb-style attacks: GNU/PAX sparse-file headers let
// Header.Size vastly exceed the physical bytes present in the stream, and
// archive/tar transparently materializes the holes as zero bytes on Read.
var errTarBomb = errors.New("go_tar: decompression bomb detected: size or ratio exceeds limits")

// GoTar extracts archive.MainFile, a plain (uncompressed) .tar file, into
// outDir using the stdlib archive/tar package. Unlike GoUnRAR/GoSevenZip,
// tar has no password-protection format, so there is no password parameter
// and no *WithPasswords wrapper -- callers dispatch directly.
//
// Go's archive/tar has zero built-in protection against path traversal,
// symlink/hardlink escapes, setuid/setgid bits, or sparse-file bombs, so
// this function implements all of them:
//   - entry paths are sanitized via SanitizeArchivePath (rejects ".."
//     components, strips leading "/"), the same helper GoUnRAR/GoSevenZip
//     use.
//   - only regular files and directories are extracted; symlink, hardlink,
//     device, and FIFO entries are skipped entirely rather than followed.
//   - setuid/setgid/sticky bits are stripped from extracted file modes
//     (only rw bits are kept, matching GoUnRAR/GoSevenZip's policy of
//     stripping all "unwanted" bits from untrusted archives).
//   - opts.MaxSize/MaxRatio/MinBombThreshold are enforced against both the
//     entry's declared Header.Size and the bytes actually read, via the
//     shared boundReader also used by GoSevenZip.
//
// It extracts into a staging directory inside outDir and publishes the result
// only on success (extractInStage).
func GoTar(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error) {
	res, err := extractInStage(log, outDir, opts, func(stageDir string) (res Result, err error) {
		err = cmdutil.SafeEngineRun("go_tar: tar panic", func() error {
			res, err = goTarInternal(ctx, log, archive, stageDir, opts)
			return err
		}, func(p any) {
			res.Reason = FailCorrupt
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: tar panic: %v", p))
			}
		})
		return res, err
	})
	res.CommandLine = fmt.Sprintf("go_tar %s -> %s", archive.MainFile, outDir)
	return res, err
}

// goTarInternal extracts archive into stageDir.
func goTarInternal(ctx context.Context, log *slog.Logger, archive Archive, stageDir string, opts Options) (res Result, err error) {
	log = log.With("component", "go_tar", "archive", archive.MainFile)

	f, err := os.Open(archive.MainFile) //nolint:gosec // read-only open of an archive path resolved by unpack.Scan
	if err != nil {
		reason := FailUnknown
		if os.IsNotExist(err) {
			reason = FailNotArchive
		}
		res.Reason = reason
		return res, fmt.Errorf("go_tar: open: %w", err)
	}
	defer f.Close() //nolint:errcheck // best-effort close, read-only

	root, err := os.OpenRoot(stageDir)
	if err != nil {
		return res, fmt.Errorf("go_tar: open root: %w", err)
	}
	defer root.Close() //nolint:errcheck // close after all writes; not in a loop

	var arcSize int64
	if st, statErr := os.Stat(archive.MainFile); statErr == nil {
		arcSize = st.Size()
	}
	var totalRead int64
	var outBuf strings.Builder

	tr := tar.NewReader(f)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, ctxErr
		}

		hdr, nextErr := tr.Next()
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				break
			}
			res.Reason = FailCorrupt
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: corrupt tar header: %v", nextErr))
			}
			return res, fmt.Errorf("go_tar: read header: %w", nextErr)
		}

		extracted, entryErr := extractTarEntry(ctx, root, stageDir, tr, hdr, opts, arcSize, &totalRead, log)
		if entryErr != nil {
			res.Reason = classifyTarError(entryErr)
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: %s: %v", hdr.Name, entryErr))
			}
			return res, entryErr
		}
		if extracted {
			outBuf.WriteString("Extracting  " + hdr.Name + "\n")
			if opts.OnLine != nil {
				opts.OnLine("Extracting  " + hdr.Name)
			}
		}
	}
	res.Output = outBuf.String()
	return res, nil
}

// extractTarEntry handles path sanitization, type filtering (regular files
// and directories only), unique naming, skip predicates, and file
// extraction for a single tar entry. root is an os.Root anchored at outDir,
// opened once by the caller. Returns true if the entry was extracted
// (regular file), or false if it was skipped or was a directory.
func extractTarEntry(ctx context.Context, root *os.Root, outDir string, tr *tar.Reader, hdr *tar.Header, opts Options, arcSize int64, totalRead *int64, log *slog.Logger) (bool, error) {
	switch hdr.Typeflag {
	case tar.TypeReg:
		// handled below
	case tar.TypeDir:
		rel, ok := memberPath(root, hdr.Name, true, opts, log)
		if !ok {
			return false, nil
		}
		if err := root.MkdirAll(rel, 0o750); err != nil {
			return false, fmt.Errorf("go_tar: mkdir %s: %w", rel, err)
		}
		return false, nil
	default:
		// Skip symlink, hardlink, character-device, block-device, and FIFO
		// entries entirely. Plain tars of Usenet content essentially never
		// need them, and this sidesteps the whole class of escape vectors:
		// a symlink/hardlink target could otherwise point outside outDir,
		// and a later entry with the same name could be written through it.
		log.Warn("skipping non-regular tar entry", "name", hdr.Name, "typeflag", hdr.Typeflag)
		if opts.OnLine != nil {
			opts.OnLine("Skipping non-regular entry: " + hdr.Name)
		}
		return false, nil
	}

	destRel, ok := memberPath(root, hdr.Name, false, opts, log)
	if !ok {
		return false, nil
	}
	destPath := filepath.Join(outDir, destRel)

	if err := extractTarFile(ctx, root, destRel, destPath, tr, hdr, opts, arcSize, totalRead, log); err != nil {
		return false, err
	}
	return true, nil
}

// extractTarFile writes a single regular-file tar entry to disk through
// root. It enforces decompression-bomb limits against both the entry's
// declared Header.Size (projected, before any bytes are read -- catches an
// oversized sparse-file claim immediately) and the bytes actually read (via
// boundReader, the same enforcement used by GoSevenZip).
func extractTarFile(ctx context.Context, root *os.Root, destRel, destPath string, tr *tar.Reader, hdr *tar.Header, opts Options, arcSize int64, totalRead *int64, log *slog.Logger) error {
	if hdr.Size < 0 {
		return fmt.Errorf("%w: entry %q has negative declared size %d", errTarBomb, hdr.Name, hdr.Size)
	}
	if err := projectedBombCheck(uint64(hdr.Size), arcSize, totalRead, opts, hdr.Name, "declared", errTarBomb, "go_tar"); err != nil {
		return err
	}

	br := newBoundReader(tr, totalRead, arcSize, opts, hdr.Name, errTarBomb, "go_tar")

	// Permissions: strip setuid/setgid/sticky bits (and, matching
	// GoUnRAR/GoSevenZip's policy for untrusted archives, all execute
	// bits) by keeping only the rw permission bits. hdr.Mode holds raw
	// Unix mode bits (0o4000/0o2000/0o1000 for setuid/setgid/sticky, plus
	// the 0o777 permission bits) -- it is not an os.FileMode, so the
	// os.ModeSetuid/os.ModeSetgid/os.ModeSticky bit constants do not apply
	// here; masking with 0o666 both strips the special bits and matches
	// the existing extractors' "only rw bits" convention.
	mode := os.FileMode(hdr.Mode & 0o666) //nolint:gosec // hdr.Mode&0o666 is bounded to valid permission bits

	_, err := writeEntrySafely(ctx, root, destRel, destPath, br, nil, true, mode, hdr.ModTime, opts, hdr.Name, "go_tar", log, nil)
	return err
}

// classifyTarError maps a go_tar extraction error to a FailReason.
func classifyTarError(err error) FailReason {
	if err == nil {
		return FailUnknown
	}
	if errors.Is(err, errTarBomb) {
		return FailCorrupt
	}
	if errors.Is(err, tar.ErrHeader) {
		return FailCorrupt
	}
	if errno, ok := errors.AsType[syscall.Errno](err); ok && errno == syscall.ENOSPC {
		return FailDiskFull
	}
	return FailUnknown
}
