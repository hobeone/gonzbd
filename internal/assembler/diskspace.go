// Package assembler writes decoded NZB articles to target files using
// seek-based out-of-order assembly. A single worker goroutine serializes all
// disk I/O so that file-handle bookkeeping requires no locking. Articles are
// written with WriteAt (pwrite on Unix), which is idempotent at a given offset
// and requires no seek-before-write dance.
package assembler

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// FreeBytes returns the number of bytes available to unprivileged processes on
// the filesystem that contains dir. It uses syscall.Statfs, which is available
// on Linux, macOS, and other Unix-like systems.
//
// Windows is not implemented — this step is Linux/macOS-only. A build-tagged
// Windows implementation (using syscall.GetDiskFreeSpaceEx) can be added later
// without changing the API.
//
// syscall.Statfs has no timeout of its own and can block in an
// uninterruptible sleep for as long as a stuck network mount (NFS/SMB)
// takes to respond — Go cannot cancel an in-flight blocking syscall, so
// ctx is honored by running the call in a background goroutine and
// returning as soon as either it completes or ctx is done. If ctx fires
// first, that goroutine is abandoned (it will exit once the syscall
// eventually returns, or never if the mount never responds) rather than
// leaving the caller's request goroutine blocked indefinitely.
//
// ctx must be non-nil and cancellable (ctx.Done() != nil). A caller passing
// context.Background() would silently convert this into an uninterruptible
// call on the caller's own goroutine — the exact failure this function's
// goroutine+select design exists to prevent — so that is rejected outright
// rather than allowed to degenerate into a plain blocking call.
func FreeBytes(ctx context.Context, dir string) (int64, error) {
	return freeBytes(ctx, dir, syscall.Statfs)
}

// freeBytes is FreeBytes' implementation, taking the statfs call as a plain
// parameter (not shared/global state) so tests can pass a controllable fake
// to exercise the goroutine-abandonment path directly — e.g. proving the
// abandoned goroutine really does complete and release its buffered channel
// once the underlying call eventually returns, rather than leaking forever.
func freeBytes(ctx context.Context, dir string, statfs func(path string, buf *syscall.Statfs_t) error) (int64, error) {
	if ctx == nil || ctx.Done() == nil {
		return 0, errors.New("assembler: FreeBytes requires a cancellable context; " +
			"statfs can block forever on an unreachable mount")
	}
	// An ALREADY-cancelled context resolves here, before the select below, and
	// that is a correctness requirement rather than a fast path. Both of the
	// select's cases can be ready at once — statfs on a local mount fills ch
	// almost immediately — and Go picks uniformly at random among ready cases,
	// so roughly half the time this reported SUCCESS for a context the caller
	// had already cancelled.
	//
	// It surfaced as TestFreeBytes_ContextCanceled failing only under parallel
	// load, which reads as a flaky test and is not: the test is right and the
	// function was wrong. Matches the pre-check convention already used by
	// WriteSpeedMBPerSec below and by CancelJob.
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("assembler: statfs %s: %w", dir, err)
	}
	type result struct {
		free int64
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		free, err := statfsFreeBytes(dir, statfs)
		ch <- result{free, err}
	}()
	select {
	case r := <-ch:
		return r.free, r.err
	case <-ctx.Done():
		return 0, fmt.Errorf("assembler: statfs %s: %w", dir, ctx.Err())
	}
}

// statfsFreeBytes performs the raw statfs call and computes the available
// byte count. Shared by freeBytes (a single ctx-bounded call) and DiskProbe
// (a detached, single-flight probe with no per-caller ctx) so both paths
// share the same Bavail*Bsize arithmetic and error wrapping.
func statfsFreeBytes(dir string, statfs func(path string, buf *syscall.Statfs_t) error) (int64, error) {
	var st syscall.Statfs_t
	if err := statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("assembler: statfs %s: %w", dir, err)
	}
	// Bavail is the number of blocks available to unprivileged processes (uint64).
	// Bsize is the fundamental block size in bytes (int64 on Linux).
	// The product fits in int64 for any sane filesystem size.
	return int64(st.Bavail) * int64(st.Bsize), nil //nolint:gosec,unconvert // G115: Bavail is filesystem metadata; unconvert: Bsize type varies by OS
}

// WriteSpeedMBPerSec writes a sizeBytes temp file into dir, times the
// write plus fsync, deletes the file, and returns the throughput in
// MB/s (decimal megabytes: bytes / 1e6 / seconds). Used by the status
// page's on-demand disk-speed-test action; ctx bounds the whole
// operation so a broken or extremely slow disk can't hang the caller
// indefinitely.
func WriteSpeedMBPerSec(ctx context.Context, dir string, sizeBytes int64) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("assembler: write speed test: %w", err)
	}

	f, err := os.CreateTemp(dir, "gonzbd-diskspeedtest-*")
	if err != nil {
		return 0, fmt.Errorf("assembler: create temp file in %s: %w", dir, err)
	}
	path := f.Name()
	defer func() {
		_ = f.Close()       //nolint:errcheck // best-effort cleanup
		_ = os.Remove(path) //nolint:errcheck // best-effort cleanup
	}()

	buf := make([]byte, 1024*1024) // 1 MiB write chunks
	// Filled with random bytes, and the first 8 bytes are overwritten with
	// the current write offset each iteration below. Either alone isn't
	// enough: a filesystem with block-level dedup (e.g. ZFS, Btrfs) would
	// still collapse a fixed random buffer written repeatedly into a
	// single physical block, and plain zeros compress or hole-punch for
	// free on any compressing filesystem — both would report throughput
	// far higher than the disk can actually sustain, defeating the point
	// of this diagnostic.
	if _, err := rand.Read(buf); err != nil {
		return 0, fmt.Errorf("assembler: write speed test: generate test data: %w", err)
	}
	start := time.Now()
	var written int64
	for written < sizeBytes {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("assembler: write speed test: %w", err)
		}
		n := int64(len(buf))
		if remaining := sizeBytes - written; remaining < n {
			n = remaining
		}
		if n >= 8 {
			binary.BigEndian.PutUint64(buf[:8], uint64(written))
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return 0, fmt.Errorf("assembler: write speed test: write %s: %w", path, err)
		}
		written += n
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("assembler: write speed test: fsync %s: %w", path, err)
	}
	elapsed := time.Since(start)
	if elapsed <= 0 {
		return 0, fmt.Errorf("assembler: write speed test: elapsed time non-positive")
	}

	mb := float64(written) / 1_000_000
	return mb / elapsed.Seconds(), nil
}
