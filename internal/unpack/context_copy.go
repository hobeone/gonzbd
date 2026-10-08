package unpack

import (
	"context"
	"io"
	"sync"
)

// contextCopyBufSize is the buffer size used by contextCopy (1 MiB).
// Pooling slices via contextCopyBufPool eliminates per-entry heap
// allocations across multi-file archives.
const contextCopyBufSize = 1 << 20

// contextCopyCheckInterval is the byte interval between ctx.Err() checks
// (256 KiB), keeping cancellation responsive when a decompressor yields
// smaller chunks per Read.
const contextCopyCheckInterval = 256 * 1024

var contextCopyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, contextCopyBufSize)
		return &b
	},
}

// contextCopy copies from src to dst while respecting ctx cancellation.
// It checks ctx.Err() every contextCopyCheckInterval (256 KiB) of data. On
// cancellation, it returns the context error immediately, allowing
// long-running copies of large files to be interrupted.
func contextCopy(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	bp, _ := contextCopyBufPool.Get().(*[]byte)
	defer contextCopyBufPool.Put(bp)
	buf := *bp

	var written int64
	var checkInterval int64 = contextCopyCheckInterval
	var sinceCheck int64

	for {
		nr, readErr := src.Read(buf)
		if nr > 0 {
			nw, writeErr := dst.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
				sinceCheck += int64(nw)
			}
			if writeErr != nil {
				return written, writeErr
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				if err := ctx.Err(); err != nil {
					return written, err
				}
				return written, nil
			}
			return written, readErr
		}

		// Periodically check for context cancellation.
		if sinceCheck >= checkInterval {
			sinceCheck = 0
			if err := ctx.Err(); err != nil {
				return written, err
			}
		}
	}
}
