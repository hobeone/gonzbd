package unpack

import (
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
	"sync/atomic"
	"syscall"

	"github.com/bodgit/sevenzip"

	"github.com/hobeone/gonzbd/internal/fsutil"
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
var (
	errSevenZipChecksum = errors.New("go_7z: checksum error: content does not match archive CRC32")
	errSevenZipBomb     = errors.New("go_7z: decompression bomb detected: size or ratio exceeds limits")
)

// GoSevenZip extracts archive.MainFile into outDir using the pure-Go
// bodgit/sevenzip library. It is equivalent to SevenZip (subprocess) but
// requires no external binary.
//
// Files are extracted in natural archive order and each file's reader is
// closed before opening the next, enabling the library's stream reuse
// optimisation for solid archives.
func GoSevenZip(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, opts Options) (res Result, err error) {
	// Top-level recover: sevenzip may panic on malformed archives.
	defer func() {
		if p := recover(); p != nil {
			res.Reason = FailCorrupt
			err = fmt.Errorf("go_7z: sevenzip panic: %v", p)
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: sevenzip panic: %v", p))
			}
		}
	}()

	log = log.With("component", "go_7z", "archive", archive.MainFile)

	// Snapshot directory before extraction.
	before, snapErr := snapshotDir(outDir)
	if snapErr != nil {
		return res, fmt.Errorf("go_7z: snapshot dir: %w", snapErr)
	}

	var r *sevenzip.ReadCloser
	if password != "" {
		r, err = sevenzip.OpenReaderWithPassword(archive.MainFile, password)
	} else {
		r, err = sevenzip.OpenReader(archive.MainFile)
	}
	if err != nil {
		res.Reason = classifySevenZipError(err)
		if opts.OnLine != nil {
			opts.OnLine(fmt.Sprintf("ERROR: failed to open archive: %v", err))
		}
		return res, fmt.Errorf("go_7z: open: %w", err)
	}
	defer r.Close() //nolint:errcheck // best-effort close

	log.Info("starting extraction",
		"outDir", outDir,
		"files", len(r.File),
	)

	// Open the output directory as an os.Root once for the entire extraction.
	// All entry writes go through this rooted handle so they cannot escape
	// outDir via "..", an absolute path, or a symlinked path component.
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return res, fmt.Errorf("go_7z: open root: %w", err)
	}
	defer root.Close() //nolint:errcheck // close after all writes; not in a loop

	var arcSize int64
	if st, statErr := os.Stat(archive.MainFile); statErr == nil {
		arcSize = st.Size()
	}
	var totalRead int64

	for _, f := range r.File {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		extracted, err := extractSevenZipEntry(ctx, f, outDir, root, opts, arcSize, &totalRead, log)
		if err != nil {
			res.Reason = classifySevenZipError(err)
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: %s: %v", f.Name, err))
			}
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				cleanupPartialFiles(outDir, before, log, "go_7z", 1)
			}
			return res, err
		}

		if extracted && opts.OnLine != nil {
			opts.OnLine("Extracting  " + f.Name)
		}
	}

	// Diff output directory to find extracted files.
	after, snapErr := snapshotDir(outDir)
	if snapErr != nil {
		return res, fmt.Errorf("go_7z: snapshot after: %w", snapErr)
	}
	res.ExtractedFiles = diffSnapshot(before, after)
	res.CommandLine = fmt.Sprintf("go_7z %s -> %s", archive.MainFile, outDir)

	var outBuf strings.Builder
	for _, f := range res.ExtractedFiles {
		displayPath := f
		if filepath.IsAbs(f) {
			if rel, err := filepath.Rel(outDir, f); err == nil && !strings.HasPrefix(rel, "..") {
				displayPath = rel
			} else {
				displayPath = filepath.Base(f)
			}
		}
		outBuf.WriteString("Extracting  " + displayPath + "\n")
	}
	res.Output = outBuf.String()

	log.Info("extraction complete",
		"extracted", len(res.ExtractedFiles))

	return res, nil
}

type boundReader struct {
	r            io.Reader
	totalRead    *int64
	maxSize      int64
	maxRatio     int64
	arcSize      int64
	minThreshold int64
	name         string
}

func (b *boundReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		atomic.AddInt64(b.totalRead, int64(n))
		currTotal := atomic.LoadInt64(b.totalRead)
		if b.maxSize > 0 && currTotal > b.maxSize {
			return n, fmt.Errorf("%w: total read (%d bytes) exceeds ceiling (%d bytes)", errSevenZipBomb, currTotal, b.maxSize)
		}
		if b.arcSize > 0 && b.maxRatio > 0 && currTotal > b.minThreshold && currTotal > b.arcSize*b.maxRatio {
			return n, fmt.Errorf("%w: archive ratio exceeds limit (%d)", errSevenZipBomb, b.maxRatio)
		}
	}
	return n, err
}

// extractSevenZipFile extracts a single file from the archive to disk through
// root, an os.Root anchored at the extraction output directory. destRel is the
// sanitized path relative to root's base directory; destPath is the absolute
// path used only for skip-check logging. The file's reader is opened and
// closed within this function to enable the library's stream reuse
// optimisation for solid archives.
func extractSevenZipFile(ctx context.Context, root *os.Root, destRel, destPath string, f *sevenzip.File, opts Options, arcSize int64, totalRead *int64, log *slog.Logger) error {
	maxSize := opts.MaxSize
	if maxSize <= 0 {
		maxSize = 250 * 1024 * 1024 * 1024 // 250 GiB
	}
	maxRatio := opts.MaxRatio
	if maxRatio <= 0 {
		maxRatio = 250
	}
	minThreshold := opts.MinBombThreshold
	if minThreshold == 0 {
		minThreshold = 10 * 1024 * 1024 // 10 MiB
	}

	currTotal := atomic.LoadInt64(totalRead)
	if currTotal < 0 {
		return fmt.Errorf("go_7z: invalid negative total read count: %d", currTotal)
	}
	projTotal := f.UncompressedSize + uint64(currTotal)
	if projTotal > uint64(maxSize) {
		return fmt.Errorf("%w: entry %q uncompressed size (%d) exceeds ceiling (%d)", errSevenZipBomb, f.Name, f.UncompressedSize, maxSize)
	}
	if arcSize > 0 && (minThreshold < 0 || projTotal > uint64(minThreshold)) && projTotal > uint64(arcSize)*uint64(maxRatio) {
		return fmt.Errorf("%w: entry %q ratio exceeds limit (%d)", errSevenZipBomb, f.Name, maxRatio)
	}

	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("go_7z: open entry %s: %w", f.Name, err)
	}
	// MUST close before the next file's Open() for stream reuse in solid archives.
	defer rc.Close() //nolint:errcheck // best-effort close

	// Handle overwrite policy.
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if !opts.OverwriteFiles {
		// Check if file already exists.
		if _, statErr := root.Stat(destRel); statErr == nil {
			log.Info("skipping existing file", "path", destPath)
			if opts.OnLine != nil {
				opts.OnLine("Skipping existing: " + f.Name)
			}
			return nil
		}
	}

	out, err := fsutil.RootedOpenFile(root, destRel, flags, 0o600)
	if err != nil {
		return fmt.Errorf("go_7z: create %s: %w", destRel, err)
	}
	defer out.Close() //nolint:errcheck // best-effort close; write errors are caught by contextCopy

	// Accumulate CRC32 over the bytes actually written so we can verify them
	// against f.CRC32 below — the library itself never does this (see
	// errSevenZipChecksum).
	hasher := crc32.NewIEEE()
	br := &boundReader{
		r:            rc,
		totalRead:    totalRead,
		maxSize:      maxSize,
		maxRatio:     maxRatio,
		arcSize:      arcSize,
		minThreshold: minThreshold,
		name:         f.Name,
	}
	if _, err := contextCopy(ctx, io.MultiWriter(out, hasher), br); err != nil {
		if errno, ok := errors.AsType[syscall.Errno](err); ok && errno == syscall.ENOSPC {
			return fmt.Errorf("go_7z: disk full writing %s: %w", destRel, err)
		}
		return fmt.Errorf("go_7z: write %s: %w", destRel, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("go_7z: close %s: %w", destRel, err)
	}

	// f.CRC32 == 0 means no digest was recorded for this entry (the
	// library's own tests treat this as "no CRC available" rather than a
	// literal zero checksum, e.g. for genuinely empty files which are
	// already handled earlier via the isEmptyStream/isEmptyFile short
	// circuit in File.Open and never reach this code path with content).
	if f.CRC32 != 0 && hasher.Sum32() != f.CRC32 {
		return fmt.Errorf("%w: file %q: computed=%08x header=%08x", errSevenZipChecksum, f.Name, hasher.Sum32(), f.CRC32)
	}

	// Permissions: strip executable bits from untrusted archives.
	// Only keep rw bits (mode & 0o666). If the archive entry has an
	// unusual execute-only mode (0111), the mask produces 0 and the
	// Chmod is skipped — the file keeps the safer 0600 from OpenFile.
	mode := f.Mode() & 0o666
	if mode != 0 {
		_ = root.Chmod(destRel, mode)
	}

	// Modification time: preserve from archive unless config says ignore.
	// Note: field is named IgnoreUnrarDates for historical reasons but
	// applies to all archive types.
	if !opts.IgnoreUnrarDates && !f.Modified.IsZero() {
		_ = root.Chtimes(destRel, f.Modified, f.Modified)
	}

	return nil
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
	if strings.Contains(errStr, "checksum error") || errors.Is(err, errSevenZipBomb) || strings.Contains(errStr, "decompression bomb") {
		return FailCorrupt
	}
	if strings.Contains(errStr, "unsupported compression algorithm") {
		return FailCorrupt
	}

	// Disk full.
	if errno, ok := errors.AsType[syscall.Errno](err); ok && errno == syscall.ENOSPC {
		return FailDiskFull
	}

	return FailUnknown
}

// extractSevenZipEntry handles path sanitization, unique naming, skip predicates,
// and file extraction for a single 7-zip archive entry. root is an os.Root
// anchored at outDir opened once by the caller. Returns true if the entry was
// extracted (regular file), or false if it was skipped or a directory.
func extractSevenZipEntry(ctx context.Context, f *sevenzip.File, outDir string, root *os.Root, opts Options, arcSize int64, totalRead *int64, log *slog.Logger) (bool, error) {
	destRel, sanitizeErr := SanitizeArchivePath(f.Name, opts.OneFolder)
	if sanitizeErr != nil {
		log.Warn("skipping entry with bad path",
			"raw_name", f.Name, "err", sanitizeErr)
		if opts.OnLine != nil {
			opts.OnLine("Skipping bad path: " + f.Name)
		}
		return false, nil
	}

	destPath := filepath.Join(outDir, destRel)

	// In OneFolder mode, different archive paths can flatten to the
	// same basename. Auto-rename to avoid silent overwrites, matching
	// 7z's -aou behavior. Skip when OverwriteFiles is true.
	if opts.OneFolder && !opts.OverwriteFiles {
		destPath = uniquePath(destPath)
		// Recompute destRel after uniquePath adjusts the absolute path.
		rel, relErr := filepath.Rel(outDir, destPath)
		if relErr != nil || !filepath.IsLocal(rel) {
			return false, fmt.Errorf("go_7z: uniquePath escaped outDir for %s", f.Name)
		}
		destRel = rel
	}

	if f.FileInfo().IsDir() {
		if err := root.MkdirAll(destRel, 0o750); err != nil {
			return false, fmt.Errorf("go_7z: mkdir %s: %w", destRel, err)
		}
		return false, nil
	}

	// Skip non-regular files: symlinks can escape outDir via relative
	// targets; device/pipe/socket entries are meaningless from archives.
	if f.Mode()&fs.ModeSymlink != 0 {
		log.Warn("skipping symlink entry", "name", f.Name)
		if opts.OnLine != nil {
			opts.OnLine("Skipping symlink: " + f.Name)
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

	if err := extractSevenZipFile(ctx, root, destRel, destPath, f, opts, arcSize, totalRead, log); err != nil {
		return false, err
	}

	return true, nil
}
