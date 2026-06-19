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

	"github.com/hobeone/gonzbd/internal/fsutil"
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

// ClassifyRarEngineError maps a rarengine error to the appropriate FailReason constant.
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

// GoUnRAREngine extracts a RAR archive using the pure-Go rarengine library.
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

	root, err := os.OpenRoot(outDir)
	if err != nil {
		return res, fmt.Errorf("go_unrar: open root: %w", err)
	}
	defer root.Close() //nolint:errcheck // close after all writes; not in a loop

	vols, err := discoverRar5Volumes(archive.MainFile)
	if err != nil {
		res.Reason = FailMissingVolume
		return res, fmt.Errorf("go_unrar: discover volumes: %w", err)
	}

	volumesChan := make(chan io.ReadCloser, len(vols))
	for _, volPath := range vols {
		vf, err := os.Open(volPath) //nolint:gosec // read-only open of discovered archive volume parts
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

	var extractedFiles []string
	var outBuf strings.Builder

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
			// Recompute destRel and verify it stays inside outDir
			rel, relErr := filepath.Rel(outDir, destPath)
			if relErr != nil || !filepath.IsLocal(rel) {
				return res, fmt.Errorf("go_unrar: uniquePath escaped outDir for %s", fh.Name)
			}
			destRel = rel
		}

		if err := ExtractEntryRarengine(ctx, root, outDir, destRel, destPath, fh, sd, opts, log); err != nil {
			res.Reason = ClassifyRarEngineError(err)
			if opts.OnLine != nil {
				opts.OnLine(fmt.Sprintf("ERROR: %s: %v", fh.Name, err))
			}
			return res, err
		}

		if !fh.IsDir {
			extractedFiles = append(extractedFiles, destPath)
			displayPath := destRel
			outBuf.WriteString("Extracting  " + displayPath + "\n")
		}

		if opts.OnLine != nil {
			opts.OnLine("Extracting  " + fh.Name)
		}
	}

	res.ExtractedFiles = extractedFiles
	res.CommandLine = fmt.Sprintf("go_unrar %s -> %s", archive.MainFile, outDir)
	res.Output = outBuf.String()

	return res, nil
}

// ExtractEntryRarengine writes a single rarengine entry through root, an
// os.Root anchored at the extraction output directory. root must be opened by
// the caller (once per extraction session) and closed after all entries have
// been written. destRel is the sanitized path relative to root's base
// directory (output of SanitizeArchivePath); destPath is the absolute path
// (filepath.Join(outDir, destRel)) used only for skip-check logging.
func ExtractEntryRarengine(ctx context.Context, root *os.Root, outDir, destRel, destPath string, fh *rarengine.FileHeader, r io.Reader, opts Options, log *slog.Logger) error {
	if fh.IsDir {
		return root.MkdirAll(destRel, 0o750)
	}

	if !opts.OverwriteFiles {
		if _, statErr := root.Stat(destRel); statErr == nil {
			log.Info("skipping existing file", "path", destPath)
			if opts.OnLine != nil {
				opts.OnLine("Skipping existing: " + fh.Name)
			}
			_, _ = io.Copy(io.Discard, r)
			return nil
		}
	}

	out, err := fsutil.RootedOpenFile(root, destRel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("go_unrar: create %s: %w", destRel, err)
	}
	defer out.Close() //nolint:errcheck // best-effort close; write errors caught by contextCopy

	if _, err := contextCopy(ctx, out, r); err != nil {
		if errno, ok := errors.AsType[syscall.Errno](err); ok && errno == syscall.ENOSPC {
			return fmt.Errorf("go_unrar: disk full writing %s: %w", destRel, err)
		}
		return fmt.Errorf("go_unrar: write %s: %w", destRel, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("go_unrar: close %s: %w", destRel, err)
	}

	mode := fh.Mode() & 0o666
	if mode != 0 && fh.HostOS != 0 {
		_ = root.Chmod(destRel, mode)
	}

	if !opts.IgnoreUnrarDates && !fh.ModificationTime.IsZero() {
		_ = root.Chtimes(destRel, fh.ModificationTime, fh.ModificationTime)
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

func discoverRar5Volumes(mainFile string) ([]string, error) {
	if !strings.Contains(mainFile, ".part") {
		return []string{mainFile}, nil
	}

	var prefix, suffix string
	var numStr strings.Builder
	var isZeroPadded bool

	idx := strings.Index(mainFile, ".part")
	if idx == -1 {
		return []string{mainFile}, nil
	}
	prefix = mainFile[:idx+5]
	remaining := mainFile[idx+5:]

	for i, c := range remaining {
		if c >= '0' && c <= '9' {
			numStr.WriteString(string(c))
		} else {
			suffix = remaining[i:]
			break
		}
	}

	if numStr.String() == "" {
		return []string{mainFile}, nil
	}

	isZeroPadded = len(numStr.String()) > 1 && numStr.String()[0] == '0'

	var volumes []string
	partNum := 1
	for {
		var volPath string
		if isZeroPadded {
			volPath = fmt.Sprintf("%s%0*d%s", prefix, len(numStr.String()), partNum, suffix)
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
