package unpack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// writeEntrySafely writes a single archive entry (already sanitized and
// type-checked by the caller) to disk through root, an os.Root anchored at
// the extraction output directory. It centralizes the logic go_tar,
// go_sevenzip, and go_unrar each independently duplicated: skip-if-exists,
// os.Root-scoped file creation (which cannot escape outDir), context-aware
// copy, and permission/timestamp restoration. It bounds nothing by size: the
// free-space check before the extraction does (checkFreeSpace).
//
// extraWriter, if non-nil, receives a copy of every byte written via
// io.MultiWriter. go_sevenzip is the only caller that uses this, to
// accumulate a CRC32 hash for verify (below) to check.
//
// The entry is written to a temporary sibling file and only published to
// destRel via an atomic rename once the write completes and verify (if
// non-nil) passes — never a direct write to destRel. This closes two gaps a
// direct write leaves open: a process crash or kill mid-write can no longer
// leave a partial file visible at destRel (the temp file is orphaned
// instead, not the published name), and a concurrent reader of outDir (e.g.
// a directory listing or another extraction pass) can never observe
// partially-written or failed-verification content under the final name.
// verify is called after the write completes and the temp file is closed,
// but before the publishing rename — go_sevenzip uses this to check its
// accumulated CRC32 against the archive's recorded digest, so a checksum
// mismatch is caught before the corrupt content is ever visible at destRel,
// not after. verify may be nil to skip this check (go_tar, go_unrar).
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
	verify func() error,
) (wrote bool, err error) {
	if !opts.OverwriteFiles {
		// Lstat, not Stat, for the reason given on fsutil.GetUniqueRelPath.
		// This one is not a containment hole — the write goes to a temp file
		// and root.Rename, which does not follow a final symlink — but Stat
		// answers about a link's TARGET, so a dangling link left by an earlier
		// entry reads as no file at all and the next entry of the same name
		// replaces it instead of being skipped, which is not what
		// OverwriteFiles=false promises.
		if _, statErr := root.Lstat(destRel); statErr == nil {
			log.Info("skipping existing file", "path", destPath)
			if opts.OnLine != nil {
				opts.OnLine("Skipping existing: " + entryName)
			}
			if drainOnSkip {
				// Use contextCopy, not a bare io.Copy, so a cancellation or
				// a genuine read failure during the drain is surfaced
				// instead of silently discarded.
				if _, err := contextCopy(ctx, io.Discard, src); err != nil {
					return false, fmt.Errorf("%s: drain skipped entry %s: %w", errPrefix, destRel, err)
				}
			}
			return false, nil
		}
	}

	out, tmpRel, err := fsutil.RootedCreateTemp(ctx, root, destRel)
	if err != nil {
		return false, fmt.Errorf("%s: create temp for %s: %w", errPrefix, destRel, err)
	}
	// published is set only once the rename below succeeds. Until then, this
	// cleans up the unpublished temp file on every return path (error or
	// panic) — destRel itself is never touched unless the write and verify
	// both succeeded. A cleanup failure is joined to the error returned.
	var published bool
	defer func() {
		if !published {
			err = errors.Join(err, fsutil.DiscardTemp(out, func() error { return root.Remove(tmpRel) }))
		}
	}()

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

	if verify != nil {
		if err := verify(); err != nil {
			return false, err
		}
	}

	if mode != 0 {
		if err := root.Chmod(tmpRel, mode); err != nil {
			log.Debug("unpack: failed to set file permissions", "file", destRel, "err", err)
		}
	}

	if !opts.IgnoreUnrarDates && !modTime.IsZero() {
		if err := root.Chtimes(tmpRel, modTime, modTime); err != nil {
			log.Debug("unpack: failed to set file timestamps", "file", destRel, "err", err)
		}
	}

	if err := root.Rename(tmpRel, destRel); err != nil {
		return false, fmt.Errorf("%s: publish %s: %w", errPrefix, destRel, err)
	}
	published = true

	return true, nil
}
