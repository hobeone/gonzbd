package unpack

import (
	"context"
	"io"
)

// contextCopy copies from src to dst while respecting ctx cancellation.
// It checks ctx.Err() every 256 KiB of data. On cancellation, it returns
// the context error immediately, allowing long-running copies of large
// files to be interrupted.
func contextCopy(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	// Use a 32 KiB buffer (same as io.Copy's default).
	buf := make([]byte, 32*1024)
	var written int64
	var checkInterval int64 = 256 * 1024 // check ctx every 256 KiB
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
