package directunpack

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// WaitFS is an fs.FS whose Open() blocks until the named volume file
// appears in the completed-volumes map, or ctx is cancelled.
//
// It is intentionally not a compliant io/fs.FS — like rardecode's own
// internal osFS, it accepts absolute paths (e.g.
// "/downloads/job/movie.part02.rar"). It resolves them by basename against
// an internal map of available volumes. It is only valid as a rardecode
// FileSystem option.
//
// CRITICAL INVARIANT: Open() must NEVER return fs.ErrNotExist while
// waiting for a volume. If rardecode receives ErrNotExist, it interprets
// it as end-of-archive (io.EOF) and silently truncates extraction.
// See rardecode/volume.go:283.
type WaitFS struct {
	mu     sync.Mutex
	avail  map[string]string // basename → absolute path
	ready  chan struct{}     // cap=1, non-blocking send on AddVolume
	ctx    context.Context
	closed bool // set by Close() to unblock all waiters
}

// NewWaitFS creates a WaitFS that blocks Open() calls until volumes are
// added via AddVolume(), or ctx is cancelled.
func NewWaitFS(ctx context.Context) *WaitFS {
	return &WaitFS{
		avail: make(map[string]string),
		ready: make(chan struct{}, 1),
		ctx:   ctx,
	}
}

// AddVolume records that a volume is now ready for extraction.
// basename is e.g. "movie.part02.rar", fullPath is the absolute path.
// Safe for concurrent use.
func (w *WaitFS) AddVolume(basename, fullPath string) {
	w.mu.Lock()
	w.avail[basename] = fullPath
	w.mu.Unlock()
	// Non-blocking signal to wake any blocked Open() call.
	select {
	case w.ready <- struct{}{}:
	default:
	}
}

// Open blocks until the named file is in avail or ctx is cancelled.
// name may be an absolute path; only the base component is matched
// against the avail map.
//
// This method NEVER returns fs.ErrNotExist — it blocks instead. This is
// critical because rardecode treats ErrNotExist as end-of-archive.
func (w *WaitFS) Open(name string) (fs.File, error) {
	base := filepath.Base(name)
	for {
		w.mu.Lock()
		path, ok := w.avail[base]
		closed := w.closed
		w.mu.Unlock()

		if ok {
			return os.Open(path) //nolint:gosec // paths from trusted internal callers
		}
		if closed {
			return nil, w.ctx.Err()
		}

		select {
		case <-w.ready:
			// A volume was added — re-check the map.
			continue
		case <-w.ctx.Done():
			return nil, w.ctx.Err()
		}
	}
}

// Close unblocks any goroutine blocked in Open(). After Close(), all
// pending and future Open() calls for unavailable volumes return the
// context error (or context.Canceled if ctx is still alive).
func (w *WaitFS) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	// Wake all blocked Open() calls by filling the signal channel.
	select {
	case w.ready <- struct{}{}:
	default:
	}
}
