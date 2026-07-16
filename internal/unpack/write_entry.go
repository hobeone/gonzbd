package unpack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// bombLimitDefaults resolves opts.MaxSize/MaxRatio/MinBombThreshold to their
// effective values, applying the same fallback defaults go_tar and
// go_sevenzip each hardcoded independently: 250 GiB absolute ceiling, 250:1
// compression-ratio ceiling, and a 10 MiB minimum-size threshold below which
// the ratio check is skipped (so a tiny archive containing a tiny file never
// false-positives on ratio alone).
func bombLimitDefaults(opts Options) (maxSize, maxRatio, minThreshold int64) {
	maxSize = opts.MaxSize
	if maxSize <= 0 {
		maxSize = 250 * 1024 * 1024 * 1024 // 250 GiB
	}
	maxRatio = opts.MaxRatio
	if maxRatio <= 0 {
		maxRatio = 250
	}
	minThreshold = opts.MinBombThreshold
	if minThreshold == 0 {
		minThreshold = 10 * 1024 * 1024 // 10 MiB
	}
	return maxSize, maxRatio, minThreshold
}

// projectedBombCheck verifies that adding entrySize to the running
// totalRead would not exceed either the absolute size ceiling or the
// compression-ratio ceiling, before any bytes of the entry are actually
// read. sizeLabel customizes the ceiling-violation message so each format
// keeps its original wording (go_tar: "declared", go_sevenzip:
// "uncompressed") since that reflects a real difference in what the number
// represents (a tar header's claimed size vs 7z's parsed uncompressed
// size).
//
// This is a projection only -- it does not itself read anything. Callers
// still wrap their reader in a *boundReader (see newBoundReader) for the
// continuous per-byte check as bytes are actually read, since a
// sparse/streamed entry's declared size can lie (see errTarBomb's doc
// comment on GNU/PAX sparse headers).
func projectedBombCheck(entrySize uint64, arcSize int64, totalRead *int64, opts Options, entryName, sizeLabel string, errBomb error, errPrefix string) error {
	maxSize, maxRatio, minThreshold := bombLimitDefaults(opts)

	currTotal := atomic.LoadInt64(totalRead)
	if currTotal < 0 {
		return fmt.Errorf("%s: invalid negative total read count: %d", errPrefix, currTotal)
	}
	projTotal := entrySize + uint64(currTotal)
	if projTotal > uint64(maxSize) { //nolint:gosec // maxSize is bombLimitDefaults' resolved value, always > 0
		return fmt.Errorf("%w: entry %q %s size (%d) exceeds ceiling (%d)", errBomb, entryName, sizeLabel, entrySize, maxSize)
	}
	// minThreshold is only converted once the "minThreshold < 0" branch of
	// the || has been ruled out, so uint64(minThreshold) is never negative
	// here; arcSize > 0 and maxRatio is bombLimitDefaults' resolved value
	// (always > 0), so both are also safe to convert.
	if arcSize > 0 && (minThreshold < 0 || projTotal > uint64(minThreshold)) && projTotal > uint64(arcSize)*uint64(maxRatio) { //nolint:gosec // see comment above
		return fmt.Errorf("%w: entry %q ratio exceeds limit (%d)", errBomb, entryName, maxRatio)
	}
	return nil
}

// newBoundReader wraps src in a boundReader configured from opts' resolved
// bomb-limit defaults, so callers don't each re-derive maxSize/maxRatio/
// minThreshold a second time (once for projectedBombCheck, once for the
// boundReader's own fields).
func newBoundReader(src io.Reader, totalRead *int64, arcSize int64, opts Options, entryName string, errBomb error, errPrefix string) *boundReader {
	maxSize, maxRatio, minThreshold := bombLimitDefaults(opts)
	return &boundReader{
		r:            src,
		totalRead:    totalRead,
		maxSize:      maxSize,
		maxRatio:     maxRatio,
		arcSize:      arcSize,
		minThreshold: minThreshold,
		name:         entryName,
		errBomb:      errBomb,
		errPrefix:    errPrefix,
	}
}

// writeEntrySafely writes a single archive entry (already sanitized and
// type-checked by the caller) to disk through root, an os.Root anchored at
// the extraction output directory. It centralizes the logic go_tar,
// go_sevenzip, and go_unrar each independently duplicated: skip-if-exists,
// os.Root-scoped file creation (which cannot escape outDir), context-aware
// copy with disk-full classification, and permission/timestamp restoration.
//
// Decompression-bomb enforcement is the caller's responsibility to apply to
// src *before* calling this function (see projectedBombCheck/
// newBoundReader) -- go_unrar's rarengine library enforces its own bomb
// limits internally (see rarengine.ErrRarBombDetected), so its call site has
// nothing to wrap src in and passes it through unwrapped.
//
// extraWriter, if non-nil, receives a copy of every byte written via
// io.MultiWriter. go_sevenzip is the only caller that uses this, to
// accumulate a CRC32 hash for its own post-write checksum verification
// (performed by the caller after this function returns, since only the
// caller has both the hasher and the archive's recorded digest).
//
// drainOnSkip controls whether src is drained (io.Copy to io.Discard) when
// an existing file is skipped under opts.OverwriteFiles=false. go_tar and
// go_unrar drain to fully consume the entry's stream position before the
// next iteration reads the next entry/volume; go_sevenzip does not drain,
// relying on its own deferred rc.Close() alone, to preserve its
// solid-archive stream-reuse optimisation across skipped entries.
//
// mode is the pre-resolved target permission bits, already masked by the
// caller to whatever it wants to keep (or 0 to skip chmod entirely -- e.g.
// go_unrar skips unless fh.HostOS != 0, a format-specific gate best decided
// by the caller). modTime is the timestamp to restore, or the zero Time to
// skip restoring it.
//
// The returned bool reports whether the entry was actually written (true)
// versus skipped because it already existed and opts.OverwriteFiles is
// false (false). go_sevenzip uses this to skip its post-write CRC32
// verification on a skip, since extraWriter (its hasher) never received any
// bytes in that case.
func writeEntrySafely(
	ctx context.Context,
	root *os.Root,
	destRel, destPath string,
	src io.Reader,
	extraWriter io.Writer,
	drainOnSkip bool,
	mode os.FileMode,
	modTime time.Time,
	opts Options,
	entryName, errPrefix string,
	log *slog.Logger,
) (bool, error) {
	if !opts.OverwriteFiles {
		if _, statErr := root.Stat(destRel); statErr == nil {
			log.Info("skipping existing file", "path", destPath)
			if opts.OnLine != nil {
				opts.OnLine("Skipping existing: " + entryName)
			}
			if drainOnSkip {
				// Use contextCopy, not a bare io.Copy, so a bomb-detection
				// error from a *boundReader-wrapped src, a cancellation, or
				// a genuine read failure during the drain is surfaced
				// instead of silently discarded — src may still enforce
				// safety limits even though this entry's bytes aren't kept.
				if _, err := contextCopy(ctx, io.Discard, src); err != nil {
					return false, fmt.Errorf("%s: drain skipped entry %s: %w", errPrefix, destRel, err)
				}
			}
			return false, nil
		}
	}

	out, err := fsutil.RootedOpenFile(root, destRel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false, fmt.Errorf("%s: create %s: %w", errPrefix, destRel, err)
	}
	defer out.Close() //nolint:errcheck // best-effort close; write errors caught by contextCopy

	var dst io.Writer = out
	if extraWriter != nil {
		dst = io.MultiWriter(out, extraWriter)
	}

	if _, err := contextCopy(ctx, dst, src); err != nil {
		if errno, ok := errors.AsType[syscall.Errno](err); ok && errno == syscall.ENOSPC {
			return false, fmt.Errorf("%s: disk full writing %s: %w", errPrefix, destRel, err)
		}
		return false, fmt.Errorf("%s: write %s: %w", errPrefix, destRel, err)
	}

	if err := out.Close(); err != nil {
		return false, fmt.Errorf("%s: close %s: %w", errPrefix, destRel, err)
	}

	if mode != 0 {
		if err := root.Chmod(destRel, mode); err != nil {
			log.Debug("unpack: failed to set file permissions", "file", destRel, "err", err)
		}
	}

	if !opts.IgnoreUnrarDates && !modTime.IsZero() {
		if err := root.Chtimes(destRel, modTime, modTime); err != nil {
			log.Debug("unpack: failed to set file timestamps", "file", destRel, "err", err)
		}
	}

	return true, nil
}
