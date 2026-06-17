package unpack

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bodgit/sevenzip"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// GoSevenZip extracts archive.MainFile into outDir using the pure-Go
// bodgit/sevenzip library. It is equivalent to SevenZip (subprocess) but
// requires no external binary.
//
// Files are extracted in natural archive order and each file's reader is
// closed before opening the next, enabling the library's stream reuse
// optimisation for solid archives.
func GoSevenZip(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (res Result, err error) {
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
	if opts.Password != "" {
		r, err = sevenzip.OpenReaderWithPassword(archive.MainFile, opts.Password)
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

	for _, f := range r.File {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		extracted, err := extractSevenZipEntry(ctx, f, outDir, root, opts, log)
		if err != nil {
			res.Reason = classifySevenZipError(err)
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: %s: %v", f.Name, err))
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

	if _, err := contextCopy(ctx, out, rc); err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == syscall.ENOSPC {
			return fmt.Errorf("go_7z: disk full writing %s: %w", destRel, err)
		}
		return fmt.Errorf("go_7z: write %s: %w", destRel, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("go_7z: close %s: %w", destRel, err)
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
	var readErr *sevenzip.ReadError
	if errors.As(err, &readErr) && readErr.Encrypted {
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

	// Disk full.
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == syscall.ENOSPC {
		return FailDiskFull
	}

	return FailUnknown
}

// extractSevenZipEntry handles path sanitization, unique naming, skip predicates,
// and file extraction for a single 7-zip archive entry. root is an os.Root
// anchored at outDir opened once by the caller. Returns true if the entry was
// extracted (regular file), or false if it was skipped or a directory.
func extractSevenZipEntry(ctx context.Context, f *sevenzip.File, outDir string, root *os.Root, opts Options, log *slog.Logger) (bool, error) {
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

	if err := extractSevenZipFile(ctx, root, destRel, destPath, f, opts, log); err != nil {
		return false, err
	}

	return true, nil
}
