package unpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bodgit/sevenzip"

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

// errSevenZipChecksum is returned when a file's decompressed content doesn't
// match the CRC32 recorded in the 7z archive header. bodgit/sevenzip parses
// this digest (exposed as File.CRC32) but never checks it during Open/Read —
// its own README FAQ tells callers to do this themselves for the
// encrypted-but-uncompressed case, and the same gap applies generally: a
// structurally valid archive whose underlying bytes are wrong (e.g.
// assembled from an incomplete download) extracts without error. The
// "checksum error" substring matches classifySevenZipError's existing
// FailCorrupt heuristic, used for the library's own checksum errors.
var errSevenZipChecksum = errors.New("go_7z: checksum error: content does not match archive CRC32")

// GoSevenZip extracts archive.MainFile into outDir using the pure-Go
// bodgit/sevenzip library. It is equivalent to SevenZip (subprocess) but
// requires no external binary.
//
// It extracts into a staging directory inside outDir and publishes the result
// only on success (extractInStage). Files are extracted in natural archive
// order and each file's reader is closed before opening the next, enabling
// the library's stream reuse optimisation for solid archives.
func GoSevenZip(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, opts Options) (Result, error) {
	res, err := extractInStage(ctx, log, archive, outDir, password, opts, func(stageDir string) (res Result, err error) {
		err = cmdutil.SafeEngineRun("go_7z: sevenzip panic", func() error {
			res, err = goSevenZipInternal(ctx, log, archive, stageDir, password, opts)
			return err
		}, func(p any) {
			res.Reason = FailCorrupt
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: sevenzip panic: %v", p))
			}
		})
		return res, err
	})
	res.CommandLine = fmt.Sprintf("go_7z %s -> %s", archive.MainFile, outDir)
	res.Engine = "go_7z"
	return res, err
}

// goSevenZipInternal extracts archive into stageDir.
func goSevenZipInternal(ctx context.Context, log *slog.Logger, archive Archive, stageDir, password string, opts Options) (res Result, err error) {
	log = log.With("component", "go_7z", "archive", archive.MainFile)

	var r *sevenzip.ReadCloser
	if password != "" {
		r, err = sevenzip.OpenReaderWithPassword(archive.MainFile, password)
	} else {
		r, err = sevenzip.OpenReader(archive.MainFile)
	}
	if err != nil {
		if strings.Contains(err.Error(), "checksum error") && isSevenZipEncrypted(archive.MainFile, password, "") {
			err = &sevenzip.ReadError{Encrypted: true, Err: err}
		}
		res.Reason = classifySevenZipError(err)
		if opts.OnLine != nil {
			opts.OnLine(fmt.Sprintf("ERROR: failed to open archive: %v", err))
		}
		return res, fmt.Errorf("go_7z: open: %w", err)
	}
	defer r.Close() //nolint:errcheck // best-effort close

	log.Info("starting extraction",
		"stageDir", stageDir,
		"files", len(r.File),
	)

	// Open the stage as an os.Root once for the entire extraction. All entry
	// writes go through this rooted handle so they cannot escape the stage
	// via "..", an absolute path, or a symlinked path component.
	root, err := os.OpenRoot(stageDir)
	if err != nil {
		return res, fmt.Errorf("go_7z: open root: %w", err)
	}
	defer root.Close() //nolint:errcheck // close after all writes; not in a loop

	var outBuf strings.Builder

	for _, f := range r.File {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		extracted, err := extractSevenZipEntry(ctx, f, stageDir, root, opts, log)
		if err != nil {
			if errors.Is(err, errSevenZipChecksum) && isSevenZipEncrypted(archive.MainFile, password, f.Name) {
				err = &sevenzip.ReadError{Encrypted: true, Err: err}
			}
			res.Reason = classifySevenZipError(err)
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: %s: %v", f.Name, err))
			}
			return res, err
		}

		if extracted {
			outBuf.WriteString("Extracting  " + f.Name + "\n")
			if opts.OnLine != nil {
				opts.OnLine("Extracting  " + f.Name)
			}
		}
	}
	res.Output = outBuf.String()
	return res, nil
}

// extractSevenZipFile extracts a single file from the archive to disk through
// root, an os.Root anchored at the extraction output directory. destRel is the
// sanitized path relative to root's base directory; destPath is the absolute
// path used only for skip-check logging. The file's reader is opened and
// closed within this function to enable the library's stream reuse
// optimisation for solid archives.
func extractSevenZipFile(ctx context.Context, root *os.Root, destRel, destPath string, f *sevenzip.File, opts Options, log *slog.Logger) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("go_7z: open entry %s: %w", f.Name, err)
	}
	// MUST close before the next file's Open() for stream reuse in solid archives.
	defer rc.Close() //nolint:errcheck // best-effort close

	// Accumulate CRC32 over the bytes actually written so we can verify them
	// against f.CRC32 below — the library itself never does this (see
	// errSevenZipChecksum).
	hasher := crc32.NewIEEE()

	// Permissions: strip executable bits from untrusted archives.
	// Only keep rw bits (mode & 0o666). If the archive entry has an
	// unusual execute-only mode (0111), the mask produces 0 and the
	// Chmod is skipped — the file keeps the safer 0600 from OpenFile.
	mode := f.Mode() & 0o666

	// verifyChecksum is called by writeEntrySafely after the write completes
	// but before the entry is published (renamed into place at destRel), so
	// a mismatch is caught before the corrupt content is ever visible under
	// its final name -- not after, as a bare post-call check would leave it.
	//
	// f.CRC32 == 0 means no digest was recorded for this entry (the
	// library's own tests treat this as "no CRC available" rather than a
	// literal zero checksum, e.g. for genuinely empty files which are
	// already handled earlier via the isEmptyStream/isEmptyFile short
	// circuit in File.Open and never reach this code path with content).
	verifyChecksum := func() error {
		if f.CRC32 != 0 && hasher.Sum32() != f.CRC32 {
			return fmt.Errorf("%w: file %q: computed=%08x header=%08x", errSevenZipChecksum, f.Name, hasher.Sum32(), f.CRC32)
		}
		return nil
	}

	// drainOnSkip=false: unlike go_tar/go_unrar, we do not drain rc on a
	// skip-existing -- the deferred rc.Close() above is enough, and not
	// draining preserves the library's stream-reuse optimisation for solid
	// archives across skipped entries.
	_, err = writeEntrySafely(ctx, root, destRel, destPath, rc, hasher, false, mode, f.Modified, opts, f.Name, "go_7z", log, verifyChecksum)
	return err
}

// classifySevenZipError maps sevenzip library errors to FailReason.
func classifySevenZipError(err error) FailReason {
	if err == nil {
		return FailUnknown
	}

	// Check for ReadError with Encrypted hint → wrong password.
	if readErr, ok := errors.AsType[*sevenzip.ReadError](err); ok && readErr.Encrypted {
		return FailWrongPassword
	}

	errStr := err.Error()

	// Format errors.
	if strings.Contains(errStr, "not a valid 7-zip file") {
		return FailNotArchive
	}
	if strings.Contains(errStr, "checksum error") {
		return FailCorrupt
	}
	if strings.Contains(errStr, "unsupported compression algorithm") {
		return FailCorrupt
	}
	return FailUnknown
}

// extractSevenZipEntry handles path sanitization, unique naming, skip predicates,
// and file extraction for a single 7-zip archive entry. root is an os.Root
// anchored at outDir opened once by the caller. Returns true if the entry was
// extracted (regular file), or false if it was skipped or a directory.
func extractSevenZipEntry(ctx context.Context, f *sevenzip.File, outDir string, root *os.Root, opts Options, log *slog.Logger) (bool, error) {
	destRel, ok := memberPath(root, f.Name, f.FileInfo().IsDir(), opts, log)
	if !ok {
		return false, nil
	}
	destPath := filepath.Join(outDir, destRel)

	if f.FileInfo().IsDir() {
		if err := root.MkdirAll(destRel, 0o750); err != nil {
			return false, fmt.Errorf("go_7z: mkdir %s: %w", destRel, err)
		}
		return false, nil
	}

	// Skip non-regular files: symlinks can escape outDir via relative
	// targets; device/pipe/socket entries are meaningless from archives.
	if f.Mode()&fs.ModeSymlink != 0 {
		log.Warn("skipping link entry: extraction creates no links in a job", "name", f.Name)
		if opts.OnLine != nil {
			opts.OnLine("Skipping link: " + f.Name)
		}
		return false, nil
	}
	if !f.FileInfo().Mode().IsRegular() {
		log.Warn("skipping non-regular entry", "name", f.Name, "mode", f.Mode())
		if opts.OnLine != nil {
			opts.OnLine("Skipping non-regular entry: " + f.Name)
		}
		return false, nil
	}

	if err := extractSevenZipFile(ctx, root, destRel, destPath, f, opts, log); err != nil {
		return false, err
	}

	return true, nil
}

// isSevenZipEncrypted probes whether the archive or entry at archivePath is
// encrypted by attempting to open and read it with an alternate password.
// It is called only on error paths (e.g. checksum mismatch) and does zero
// work on successful extractions.
func isSevenZipEncrypted(archivePath, password, entryName string) bool {
	altPassword := password + "\x00"
	altR, err := sevenzip.OpenReaderWithPassword(archivePath, altPassword)
	if err != nil {
		if readErr, ok := errors.AsType[*sevenzip.ReadError](err); ok && readErr.Encrypted {
			return true
		}
		return false
	}
	defer altR.Close() //nolint:errcheck // best-effort close

	r, err := sevenzip.OpenReaderWithPassword(archivePath, password)
	if err != nil {
		return false
	}
	defer r.Close() //nolint:errcheck // best-effort close

	var f1, f2 *sevenzip.File
	if entryName != "" {
		for _, f := range r.File {
			if f.Name == entryName {
				f1 = f
				break
			}
		}
		for _, f := range altR.File {
			if f.Name == entryName {
				f2 = f
				break
			}
		}
	} else {
		for _, f := range r.File {
			if !f.FileInfo().IsDir() && f.UncompressedSize > 0 {
				f1 = f
				for _, af := range altR.File {
					if af.Name == f.Name {
						f2 = af
						break
					}
				}
				break
			}
		}
	}

	if f1 == nil || f2 == nil {
		return false
	}

	rc1, err := f1.Open()
	if err != nil {
		if readErr, ok := errors.AsType[*sevenzip.ReadError](err); ok && readErr.Encrypted {
			return true
		}
		return false
	}
	defer rc1.Close() //nolint:errcheck // best-effort close

	rc2, err := f2.Open()
	if err != nil {
		if readErr, ok := errors.AsType[*sevenzip.ReadError](err); ok && readErr.Encrypted {
			return true
		}
		return false
	}
	defer rc2.Close() //nolint:errcheck // best-effort close

	buf1 := make([]byte, 16)
	buf2 := make([]byte, 16)

	n1, _ := io.ReadFull(rc1, buf1) //nolint:errcheck // best-effort read up to 16 bytes for comparison
	n2, _ := io.ReadFull(rc2, buf2) //nolint:errcheck // best-effort read up to 16 bytes for comparison

	return n1 > 0 && n2 > 0 && !bytes.Equal(buf1[:n1], buf2[:n2])
}
