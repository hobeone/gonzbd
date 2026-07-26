package unpack

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/hobeone/rarengine"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/rarheader"
)

// GoUnRAR extracts archive.MainFile into outDir using pure-Go rarengine.
// It does not fall back to the external unrar binary itself — that decision
// belongs to the caller (see stage_unpack.go's GoRarFallback-gated retry),
// so a Result's Engine/CommandLine always accurately reflect which tool
// actually ran, and so opts.GoRarFallback=false reliably means "never shell
// out," with no other path silently overriding it.
// It is equivalent to UnRAR.
func GoUnRAR(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, opts Options) (res Result, err error) {
	ver, detectErr := rarheader.Version(archive.MainFile)
	if detectErr != nil {
		reason := FailUnknown
		if errors.Is(detectErr, rarheader.ErrNotRAR) {
			reason = FailNotArchive
		}
		log.Info("go_unrar: cannot determine RAR version", "err", detectErr)
		return Result{Reason: reason}, fmt.Errorf("go_unrar: %w", detectErr)
	}

	log.Info("go_unrar: detected RAR archive version, using rarengine", "rar_version", ver)
	beforeSnap, snapErr := snapshotDir(outDir)
	res, err = GoUnRAREngine(ctx, log, archive, outDir, password, opts)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && snapErr == nil {
		cleanupPartialFiles(outDir, beforeSnap, log, "go_unrar_rarengine", 1)
	}
	return res, err
}

// ClassifyRarEngineError maps a rarengine error to the appropriate FailReason constant.
func ClassifyRarEngineError(err error) FailReason {
	switch {
	case errors.Is(err, rarengine.ErrWrongPassword), errors.Is(err, rarengine.ErrPasswordRequired):
		return FailWrongPassword
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
func GoUnRAREngine(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, opts Options) (res Result, err error) {
	err = cmdutil.SafeEngineRun("go_unrar: rarengine panic", func() error {
		res, err = goUnRAREngineInternal(ctx, log, archive, outDir, password, opts)
		return err
	}, func(p any) {
		res.Reason = FailCorrupt
		if opts.OnLine != nil {
			opts.OnLine(fmt.Sprintf("ERROR: rarengine panic: %v", p))
		}
	})
	return res, err
}

func goUnRAREngineInternal(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, opts Options) (res Result, err error) {
	log = log.With("component", "go_unrar_engine", "archive", archive.MainFile)

	root, err := os.OpenRoot(outDir)
	if err != nil {
		return res, fmt.Errorf("go_unrar: open root: %w", err)
	}
	defer root.Close() //nolint:errcheck // close after all writes; not in a loop

	vols, err := volumesForArchive(archive)
	if err != nil {
		res.Reason = FailMissingVolume
		return res, fmt.Errorf("go_unrar: discover volumes: %w", err)
	}

	var openedVols []io.ReadCloser
	defer func() {
		for _, v := range openedVols {
			_ = v.Close() //nolint:errcheck // best-effort close on completion or early exit
		}
	}()

	volumesChan := make(chan io.ReadCloser, len(vols))
	for _, volPath := range vols {
		vf, err := os.Open(volPath) //nolint:gosec // read-only open of discovered archive volume parts
		if err != nil {
			close(volumesChan)
			res.Reason = FailMissingVolume
			return res, fmt.Errorf("go_unrar: open volume %q: %w", volPath, err)
		}
		openedVols = append(openedVols, vf)
		volumesChan <- vf
	}
	close(volumesChan)

	sd := rarengine.NewStreamDecompressor(volumesChan)
	if password != "" {
		sd.SetPassword(password)
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

		sp, sanitizeErr := NewSanitizedPath(fh.Name, opts.OneFolder)
		if sanitizeErr != nil {
			log.Warn("skipping entry with bad path", "raw_name", fh.Name, "err", sanitizeErr)
			if opts.OnLine != nil {
				opts.OnLine("Skipping bad path: " + fh.Name)
			}
			_, _ = io.Copy(io.Discard, sd) //nolint:errcheck // drain stream to skip bad entry
			continue
		}

		destRel := sp.Rel()
		destPath := sp.Abs(outDir)

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

	// mode is masked to only the rw bits, matching go_tar/go_sevenzip's
	// policy for untrusted archives -- but also gated on fh.HostOS != 0:
	// rarengine reports Mode()==0 for archives created on hosts that don't
	// carry Unix permission bits (e.g. Windows-authored RARs), and chmod-ing
	// to 0 there would strip the safer 0600 default from OpenFile for no
	// reason, so mode is forced to 0 (skip chmod) in that case too.
	mode := fh.Mode() & 0o666
	if fh.HostOS == 0 {
		mode = 0
	}

	// No bomb-limit check here: rarengine enforces its own decompression-
	// bomb limits internally (see rarengine.ErrRarBombDetected, surfaced via
	// ClassifyRarEngineError), so r is passed through to writeEntrySafely
	// unwrapped, unlike go_tar/go_sevenzip which wrap their reader in a
	// boundReader before writing.
	_, err := writeEntrySafely(ctx, root, destRel, destPath, r, nil, true, mode, fh.ModificationTime, opts, fh.Name, "go_unrar", log, nil)
	return err
}

// volumesForArchive returns the ordered list of volume files belonging to
// archive, for streaming into rarengine. archive.Parts (populated by Scan)
// already has the correct file set when available; otherwise this probes
// the filesystem starting from MainFile (e.g. for hand-built Archive values
// that bypass Scan, such as in tests). Either way, the candidate set is run
// through orderVolumes -- the single place that knows how to sequence
// volumes correctly, so there is exactly one algorithm regardless of how
// the candidates were discovered.
func volumesForArchive(archive Archive) ([]string, error) {
	if len(archive.Parts) > 1 {
		return orderVolumes(archive.MainFile, archive.Parts), nil
	}
	discovered, err := discoverRar5Volumes(archive.MainFile)
	if err != nil {
		return nil, err
	}
	return orderVolumes(archive.MainFile, discovered), nil
}

// orderVolumes sequences an unordered (or deletion-ordered) set of volume
// file paths into the order rarengine must read them in: mainFile first,
// then any legacy ".rNN" volumes by ascending numeric suffix. New-style
// ".partNN.rar" sets are already in correct lexicographic order and are
// returned unchanged. This is the only place that reorders volumes --
// archive.Parts is sorted for deletion purposes, not playback order, since
// legacy ".r00" sorts lexicographically before the main ".rar" volume,
// which would feed rarengine the volumes out of order and make it abort
// partway through with "channel was closed" once it expects a next volume
// that was never queued in the right place.
func orderVolumes(mainFile string, candidates []string) []string {
	if len(candidates) <= 1 {
		return candidates
	}

	type numbered struct {
		n    int
		path string
	}
	var legacy []numbered
	for _, p := range candidates {
		if p == mainFile {
			continue
		}
		base := strings.ToLower(filepath.Base(p))
		m := legacyExtraPattern.FindString(base)
		if m == "" {
			// Not legacy ".rNN" naming (e.g. new-style ".partNN.rar",
			// already in correct lexicographic order) -- trust the
			// existing order.
			return candidates
		}
		n, err := strconv.Atoi(m[2:]) // strip leading ".r"
		if err != nil {
			// Unreachable in practice: legacyExtraPattern guarantees digits
			// after ".r". Trust the existing order rather than failing
			// extraction outright over a defensive case.
			return candidates //nolint:nilerr // intentional fallback, not an oversight
		}
		legacy = append(legacy, numbered{n, p})
	}
	slices.SortFunc(legacy, func(a, b numbered) int { return cmp.Compare(a.n, b.n) })

	out := make([]string, 0, len(candidates))
	out = append(out, mainFile)
	for _, item := range legacy {
		out = append(out, item.path)
	}
	return out
}

// discoverRar5Volumes probes the filesystem for sibling volumes of mainFile.
// Used only when no caller-supplied candidate list (Archive.Parts) is
// available -- e.g. hand-built Archive values that bypass Scan. Recognizes
// both volume-naming conventions Scan itself understands: new-style
// ".partNN.rar" and legacy ".rar"/".r00"/".r01"/….
func discoverRar5Volumes(mainFile string) ([]string, error) {
	if strings.Contains(mainFile, ".part") {
		return discoverPartNNVolumes(mainFile)
	}
	if strings.HasSuffix(strings.ToLower(mainFile), ".rar") {
		return discoverLegacyVolumes(mainFile)
	}
	return []string{mainFile}, nil
}

// discoverLegacyVolumes probes for ".r00", ".r01", … siblings of a legacy
// mainFile ending in ".rar", stopping at the first missing index.
func discoverLegacyVolumes(mainFile string) ([]string, error) {
	volumes := []string{mainFile}
	base := mainFile[:len(mainFile)-len(".rar")]
	for n := 0; ; n++ {
		volPath := fmt.Sprintf("%s.r%02d", base, n)
		if _, err := os.Stat(volPath); err != nil {
			if os.IsNotExist(err) {
				break
			}
			return nil, err
		}
		volumes = append(volumes, volPath)
	}
	return volumes, nil
}

func discoverPartNNVolumes(mainFile string) ([]string, error) {
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
