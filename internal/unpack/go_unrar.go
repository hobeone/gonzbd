package unpack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hobeone/rarengine"

	"github.com/hobeone/gonzbd/internal/rarheader"
)

// GoUnRAR extracts archive.MainFile into outDir using pure-Go rarengine,
// or falls back to the external unrar command if rarengine fails or the
// archive isn't a RAR3/RAR5 archive rarengine can read.
// It is equivalent to UnRAR.
func GoUnRAR(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (res Result, err error) {
	ver, detectErr := rarheader.Version(archive.MainFile)
	if detectErr == nil && (ver == 3 || ver == 5) {
		log.Info("go_unrar: detected RAR archive version, using rarengine", "rar_version", ver)
		beforeSnap, snapErr := snapshotDir(outDir)
		var rarengineRes Result
		rarengineRes, err = GoUnRAREngine(ctx, log, archive, outDir, opts)
		if err == nil {
			return rarengineRes, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return rarengineRes, err
		}
		log.Warn("go_unrar: rarengine failed, falling back to external unrar", "rar_version", ver, "err", err)
		if snapErr == nil {
			cleanupPartialFiles(outDir, beforeSnap, log, "go_unrar_rarengine", 1)
		}
	} else {
		log.Info("go_unrar: archive is not a recognized RAR version, falling back to external unrar")
	}

	return UnRAR(ctx, log, archive, outDir, opts)
}

func ClassifyRarEngineError(err error) FailReason {
	switch {
	case errors.Is(err, rarengine.ErrRarBombDetected):
		return FailCorrupt
	case errors.Is(err, rarengine.ErrBadHeaderCRC):
		return FailCorrupt
	case errors.Is(err, rarengine.ErrNoNextVolume):
		return FailMissingVolume
	default:
		return FailUnknown
	}
}

func GoUnRAREngine(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			res.Reason = FailCorrupt
			err = fmt.Errorf("go_unrar: rarengine panic: %v", p)
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: rarengine panic: %v", p))
			}
		}
	}()

	log = log.With("component", "go_unrar_engine", "archive", archive.MainFile)

	unpackOpts := rarengine.UnpackOptions{
		Password:         opts.Password,
		Logger:           log,
		OneFolder:        opts.OneFolder,
		OverwriteFiles:   opts.OverwriteFiles,
		IgnoreUnrarDates: opts.IgnoreUnrarDates,
		OnEntry: func(fh *rarengine.FileHeader) {
			if opts.OnLine != nil {
				opts.OnLine("Extracting  " + fh.Name)
			}
		},
	}

	files, err := rarengine.UnpackDir(ctx, archive.MainFile, outDir, unpackOpts)
	if err != nil {
		res.Reason = ClassifyRarEngineError(err)
		return res, fmt.Errorf("go_unrar: unpack: %w", err)
	}

	res.ExtractedFiles = files
	res.CommandLine = fmt.Sprintf("go_unrar %s -> %s", archive.MainFile, outDir)

	var outBuf strings.Builder
	for _, f := range files {
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

	return res, nil
}

func ExtractEntryRarengine(ctx context.Context, outDir, destPath string, fh *rarengine.FileHeader, r io.Reader, opts Options, log *slog.Logger) error {
	if fh.IsDir {
		return os.MkdirAll(destPath, 0o750)
	}

	if !opts.OverwriteFiles {
		if _, statErr := os.Stat(destPath); statErr == nil {
			log.Info("skipping existing file", "path", destPath)
			if opts.OnLine != nil {
				opts.OnLine("Skipping existing: " + fh.Name)
			}
			_, _ = io.Copy(io.Discard, r)
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("go_unrar: mkdir %s: %w", filepath.Dir(destPath), err)
	}

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("go_unrar: create %s: %w", destPath, err)
	}
	defer out.Close()

	if _, err := contextCopy(ctx, out, r); err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == syscall.ENOSPC {
			return fmt.Errorf("go_unrar: disk full writing %s: %w", destPath, err)
		}
		return fmt.Errorf("go_unrar: write %s: %w", destPath, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("go_unrar: close %s: %w", destPath, err)
	}

	mode := fh.Mode() & 0o666
	if mode != 0 && fh.HostOS != 0 {
		_ = os.Chmod(destPath, mode)
	}

	if !opts.IgnoreUnrarDates && !fh.ModificationTime.IsZero() {
		_ = os.Chtimes(destPath, fh.ModificationTime, fh.ModificationTime)
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
		for component := range strings.SplitSeq(name, "/") {
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
