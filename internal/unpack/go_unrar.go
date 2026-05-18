package unpack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	rardecode "github.com/nwaples/rardecode/v2"
)

// GoUnRAR extracts archive.MainFile into outDir using pure-Go rardecode.
// It is equivalent to UnRAR but requires no external binary.
func GoUnRAR(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (res Result, err error) {
	// Top-level recover: rardecode panics on some malformed archives.
	defer func() {
		if p := recover(); p != nil {
			res.Reason = FailCorrupt
			err = fmt.Errorf("go_unrar: rardecode panic: %v", p)
		}
	}()

	log = log.With("component", "go_unrar", "archive", archive.MainFile)

	// Snapshot directory before extraction.
	before, snapErr := snapshotDir(outDir)
	if snapErr != nil {
		return res, fmt.Errorf("go_unrar: snapshot dir: %w", snapErr)
	}

	rdOpts := []rardecode.Option{
		rardecode.MaxDictionarySize(512 << 20), // cap at 512 MiB — malformed archives can claim 4 GiB
	}
	if opts.Password != "" {
		rdOpts = append(rdOpts, rardecode.Password(opts.Password))
	}

	r, err := SafeOpenReader(archive.MainFile, rdOpts...)
	if err != nil {
		res.Reason = ClassifyRarDecodeError(err)
		return res, fmt.Errorf("go_unrar: open: %w", err)
	}
	defer r.Close()

	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		hdr, err := SafeNext(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			res.Reason = ClassifyRarDecodeError(err)
			return res, fmt.Errorf("go_unrar: read header: %w", err)
		}

		destRel, sanitizeErr := SanitizeArchivePath(hdr.Name, opts.OneFolder)
		if sanitizeErr != nil {
			log.Warn("go_unrar: skipping entry with bad path",
				"raw_name", hdr.Name, "err", sanitizeErr)
			// Drain the reader for this entry to advance to the next.
			_, _ = io.Copy(io.Discard, r)
			continue
		}

		destPath := filepath.Join(outDir, destRel)

		// In OneFolder mode, different archive paths can flatten to the
		// same basename. Auto-rename to avoid silent overwrites, matching
		// unrar's -or behavior. Skip when OverwriteFiles is true.
		if opts.OneFolder && !opts.OverwriteFiles {
			destPath = uniquePath(destPath)
		}

		if err := ExtractEntry(ctx, outDir, destPath, hdr, r, opts, log); err != nil {
			res.Reason = ClassifyRarDecodeError(err)
			return res, err
		}

		if opts.OnLine != nil {
			opts.OnLine("Extracting  " + hdr.Name)
		}
	}

	// Diff output directory to find extracted files.
	after, snapErr := snapshotDir(outDir)
	if snapErr != nil {
		return res, fmt.Errorf("go_unrar: snapshot after: %w", snapErr)
	}
	res.ExtractedFiles = diffSnapshot(before, after)
	res.CommandLine = fmt.Sprintf("go_unrar %s -> %s", archive.MainFile, outDir)

	log.Info("extraction complete",
		"extracted", len(res.ExtractedFiles))

	return res, nil
}

// SafeOpenReader wraps rardecode.OpenReader with panic recovery.
// Malformed archives can panic inside rardecode's block header parser.
func SafeOpenReader(name string, opts ...rardecode.Option) (r *rardecode.ReadCloser, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("go_unrar: rardecode panic on open %s: %v", filepath.Base(name), p)
		}
	}()
	return rardecode.OpenReader(name, opts...)
}

// SafeNext wraps r.Next() with panic recovery.
// rardecode may panic on certain malformed file headers.
func SafeNext(r *rardecode.ReadCloser) (hdr *rardecode.FileHeader, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("go_unrar: rardecode panic on Next(): %v", p)
		}
	}()
	return r.Next()
}

// ExtractEntry writes one file from the archive to disk.
func ExtractEntry(ctx context.Context, outDir, destPath string, hdr *rardecode.FileHeader, r io.Reader, opts Options, log *slog.Logger) error {
	// Directory entries: create and skip.
	if hdr.IsDir {
		return os.MkdirAll(destPath, 0750)
	}

	// Skip non-regular files: symlinks can escape outDir via relative
	// targets; device/pipe/socket entries are meaningless from archives.
	if hdr.Mode()&fs.ModeSymlink != 0 {
		log.Warn("go_unrar: skipping symlink entry", "name", hdr.Name)
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	if !hdr.Mode().IsRegular() && !hdr.IsDir {
		log.Warn("go_unrar: skipping non-regular entry", "name", hdr.Name, "mode", hdr.Mode())
		_, _ = io.Copy(io.Discard, r)
		return nil
	}

	// Create parent directories.
	if err := os.MkdirAll(filepath.Dir(destPath), 0750); err != nil {
		return fmt.Errorf("go_unrar: mkdir %s: %w", filepath.Dir(destPath), err)
	}

	// Handle overwrite policy.
	if !opts.OverwriteFiles {
		if _, statErr := os.Stat(destPath); statErr == nil {
			log.Info("go_unrar: skipping existing file", "path", destPath)
			_, _ = io.Copy(io.Discard, r) // drain the entry to advance the reader
			return nil
		}
	}

	// Write file.
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("go_unrar: create %s: %w", destPath, err)
	}
	defer out.Close()

	if _, err := contextCopy(ctx, out, r); err != nil {
		// Check for disk full.
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == syscall.ENOSPC {
			return fmt.Errorf("go_unrar: disk full writing %s: %w", destPath, err)
		}
		return fmt.Errorf("go_unrar: write %s: %w", destPath, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("go_unrar: close %s: %w", destPath, err)
	}

	// Permissions: strip executable bits from untrusted archives.
	// Only keep rw for user/group/other (mode & 0666).
	mode := hdr.Mode() & 0666
	if mode != 0 && hdr.HostOS != rardecode.HostOSUnknown {
		_ = os.Chmod(destPath, mode)
	}

	// Modification time: preserve from archive unless config says ignore.
	// Use archive mtime for both atime and mtime for consistency with
	// go_sevenzip (I2 fix).
	if !opts.IgnoreUnrarDates && !hdr.ModificationTime.IsZero() {
		_ = os.Chtimes(destPath, hdr.ModificationTime, hdr.ModificationTime)
	}

	return nil
}

// SanitizeArchivePath cleans an archive entry path for safe extraction.
// It prevents directory traversal attacks, rejects null bytes, and optionally
// flattens paths (OneFolder mode).
func SanitizeArchivePath(name string, oneFolder bool) (string, error) {
	// Normalize separators.
	name = strings.ReplaceAll(name, "\\", "/")

	// Reject null bytes.
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("archive path contains null byte: %q", name)
	}

	if oneFolder {
		name = path.Base(name)
	} else {
		// Clean and strip leading traversal components.
		name = path.Clean(name)
		name = strings.TrimPrefix(name, "/")
		// Reject any remaining ".." path component after cleaning.
		// path.Clean resolves internal ".." (a/../b → b), so ".." can
		// only survive at the start after cleaning. This broader check
		// is defense-in-depth against exotic path encodings.
		for _, component := range strings.Split(name, "/") {
			if component == ".." {
				return "", fmt.Errorf("archive path escapes output directory: %q", name)
			}
		}
	}

	if name == "" || name == "." {
		return "", fmt.Errorf("archive path is empty after sanitization")
	}
	return name, nil
}

// ClassifyRarDecodeError maps rardecode errors to FailReason.
func ClassifyRarDecodeError(err error) FailReason {
	switch {
	// Wrong password (RAR5 only — see TODO below).
	case errors.Is(err, rardecode.ErrBadPassword):
		return FailWrongPassword
	// Encrypted archive/files without a password provided.
	case errors.Is(err, rardecode.ErrArchiveEncrypted),
		errors.Is(err, rardecode.ErrArchivedFileEncrypted):
		return FailWrongPassword

	// Corrupt archive — various rardecode error types.
	// TODO(rardecode): ErrBadFileChecksum also fires for wrong-password on
	// RAR3/4 archives. rardecode v2.2.2 only has ErrBadPassword for RAR5.
	// Investigate adding RAR3 wrong-password detection upstream.
	case errors.Is(err, rardecode.ErrBadFileChecksum),
		errors.Is(err, rardecode.ErrCorruptBlockHeader),
		errors.Is(err, rardecode.ErrCorruptFileHeader),
		errors.Is(err, rardecode.ErrBadHeaderCRC),
		errors.Is(err, rardecode.ErrHuffDecodeFailed),
		errors.Is(err, rardecode.ErrCorruptPPM),
		errors.Is(err, rardecode.ErrShortFile),
		errors.Is(err, rardecode.ErrDecoderOutOfData),
		errors.Is(err, rardecode.ErrCorruptEncryptData),
		errors.Is(err, rardecode.ErrDictionaryTooLarge):
		return FailCorrupt

	// Not a RAR archive.
	case errors.Is(err, rardecode.ErrNoSig),
		errors.Is(err, rardecode.ErrUnknownVersion):
		return FailNotArchive

	default:
		// Disk full: check syscall.ENOSPC.
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == syscall.ENOSPC {
			return FailDiskFull
		}
		// Missing volume: rardecode returns fs.ErrNotExist when next volume absent.
		if errors.Is(err, fs.ErrNotExist) {
			return FailMissingVolume
		}
		return FailUnknown
	}
}
