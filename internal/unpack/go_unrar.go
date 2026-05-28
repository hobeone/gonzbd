package unpack

import (
	"bytes"
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
)

// GoUnRAR extracts archive.MainFile into outDir using pure-Go rarengine,
// or falls back to the external unrar command if rarengine fails or is not RAR5.
// It is equivalent to UnRAR.
func GoUnRAR(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (res Result, err error) {
	isRar5, detectErr := detectRar5(archive.MainFile)
	if detectErr == nil && isRar5 {
		beforeSnap, snapErr := snapshotDir(outDir)
		var rarengineRes Result
		rarengineRes, err = GoUnRAREngine(ctx, log, archive, outDir, opts)
		if err == nil {
			return rarengineRes, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return rarengineRes, err
		}
		log.Warn("go_unrar: rarengine failed, falling back to external unrar", "err", err)
		if snapErr == nil {
			cleanupPartialFiles(outDir, beforeSnap, log, "go_unrar_rarengine", 1)
		}
	} else {
		log.Info("go_unrar: archive is not RAR5, falling back to external unrar")
	}

	return UnRAR(ctx, log, archive, outDir, opts)
}

func detectRar5(filename string) (bool, error) {
	f, err := os.Open(filename)
	if err != nil {
		return false, err
	}
	defer f.Close()

	var magic [8]byte
	n, err := f.Read(magic[:])
	if err != nil && err != io.EOF {
		return false, err
	}
	if n < 8 {
		return false, nil
	}
	expectedMagic := []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x01, 0x00}
	return bytes.Equal(magic[:], expectedMagic), nil
}

func discoverRar5Volumes(mainFile string) ([]string, error) {
	if !strings.Contains(mainFile, ".part") {
		return []string{mainFile}, nil
	}

	var prefix, suffix string
	var numStr string
	var isZeroPadded bool

	idx := strings.Index(mainFile, ".part")
	if idx == -1 {
		return []string{mainFile}, nil
	}
	prefix = mainFile[:idx+5]
	remaining := mainFile[idx+5:]

	for i, c := range remaining {
		if c >= '0' && c <= '9' {
			numStr += string(c)
		} else {
			suffix = remaining[i:]
			break
		}
	}

	if numStr == "" {
		return []string{mainFile}, nil
	}

	isZeroPadded = len(numStr) > 1 && numStr[0] == '0'

	var volumes []string
	partNum := 1
	for {
		var volPath string
		if isZeroPadded {
			volPath = fmt.Sprintf("%s%0*d%s", prefix, len(numStr), partNum, suffix)
		} else {
			volPath = fmt.Sprintf("%s%d%s", prefix, partNum, suffix)
		}

		if _, err := os.Stat(volPath); err != nil {
			if os.IsNotExist(err) {
				if partNum > 1 {
					break
				}
				volPath = mainFile
				if _, err := os.Stat(volPath); err == nil {
					volumes = append(volumes, volPath)
				}
				break
			}
			return nil, err
		}
		volumes = append(volumes, volPath)
		partNum++
	}

	return volumes, nil
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

	before, snapErr := snapshotDir(outDir)
	if snapErr != nil {
		return res, fmt.Errorf("go_unrar: snapshot dir: %w", snapErr)
	}

	vols, err := discoverRar5Volumes(archive.MainFile)
	if err != nil {
		res.Reason = FailMissingVolume
		return res, fmt.Errorf("go_unrar: discover volumes: %w", err)
	}

	volumesChan := make(chan io.ReadCloser, len(vols))
	for _, volPath := range vols {
		vf, err := os.Open(volPath)
		if err != nil {
			close(volumesChan)
			for v := range volumesChan {
				_ = v.Close()
			}
			res.Reason = FailMissingVolume
			return res, fmt.Errorf("go_unrar: open volume %q: %w", volPath, err)
		}
		volumesChan <- vf
	}
	close(volumesChan)

	sd := rarengine.NewStreamDecompressor(volumesChan)
	if opts.Password != "" {
		sd.SetPassword(opts.Password)
	}

	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		fh, err := sd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, rarengine.ErrNoNextVolume) {
				break
			}
			res.Reason = ClassifyRarEngineError(err)
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: corrupt archive header: %v", err))
			}
			return res, fmt.Errorf("go_unrar: read header: %w", err)
		}

		destRel, sanitizeErr := SanitizeArchivePath(fh.Name, opts.OneFolder)
		if sanitizeErr != nil {
			log.Warn("skipping entry with bad path", "raw_name", fh.Name, "err", sanitizeErr)
			if opts.OnLine != nil {
				opts.OnLine("Skipping bad path: " + fh.Name)
			}
			_, _ = io.Copy(io.Discard, sd)
			continue
		}

		destPath := filepath.Join(outDir, destRel)

		if opts.OneFolder && !opts.OverwriteFiles {
			destPath = uniquePath(destPath)
		}

		if err := ExtractEntryRarengine(ctx, outDir, destPath, fh, sd, opts, log); err != nil {
			res.Reason = ClassifyRarEngineError(err)
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: %s: %v", fh.Name, err))
			}
			return res, err
		}

		if opts.OnLine != nil {
			opts.OnLine("Extracting  " + fh.Name)
		}
	}

	after, snapErr := snapshotDir(outDir)
	if snapErr != nil {
		return res, fmt.Errorf("go_unrar: snapshot after: %w", snapErr)
	}
	res.ExtractedFiles = diffSnapshot(before, after)
	res.CommandLine = fmt.Sprintf("go_unrar %s -> %s", archive.MainFile, outDir)

	log.Info("rarengine: extraction complete", "extracted", len(res.ExtractedFiles))

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
