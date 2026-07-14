// Package assembler writes decoded NZB articles to target files using
// seek-based out-of-order assembly. A single worker goroutine serializes all
// disk I/O so that file-handle bookkeeping requires no locking. Articles are
// written with WriteAt (pwrite on Unix), which is idempotent at a given offset
// and requires no seek-before-write dance.
package assembler

import (
	"context"
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
func FreeBytes(ctx context.Context, dir string) (int64, error) {
	type result struct {
		free int64
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			ch <- result{0, fmt.Errorf("assembler: statfs %s: %w", dir, err)}
			return
		}
		// Bavail is the number of blocks available to unprivileged processes (uint64).
		// Bsize is the fundamental block size in bytes (int64 on Linux).
		// The product fits in int64 for any sane filesystem size.
		ch <- result{int64(st.Bavail) * int64(st.Bsize), nil} //nolint:gosec,unconvert // G115: Bavail is filesystem metadata; unconvert: Bsize type varies by OS
	}()
	select {
	case r := <-ch:
		return r.free, r.err
	case <-ctx.Done():
		return 0, fmt.Errorf("assembler: statfs %s: %w", dir, ctx.Err())
	}
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
