package assembler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

// defaultQueueSize mirrors DEF_MAX_ASSEMBLER_QUEUE from the Python source.
const defaultQueueSize = 12

// diskCheckInterval is how many WriteRequests the worker processes between
// disk-space checks. Checking every request would dominate the syscall budget
// on fast I/O paths; every 16 is a reasonable amortization.
const diskCheckInterval = 16

var (
	// ErrNotStarted is returned by WriteArticle when Start has not yet been called.
	ErrNotStarted = errors.New("assembler: not started")

	// ErrStopped is returned by WriteArticle after Stop has been called.
	ErrStopped = errors.New("assembler: stopped")

	// ErrFileInfo is returned by the worker when the FileInfo resolver fails.
	// The caller's WriteArticle call has already returned nil (the request was
	// enqueued); this error is logged inside the worker, not propagated upstream.
	ErrFileInfo = errors.New("assembler: FileInfo resolver failed")
)

// WriteRequest is the unit of work sent to the assembler. Each request
// corresponds to one decoded NZB article segment.
type WriteRequest struct {
	// JobID identifies the parent download job.
	JobID string

	// FileIdx is the index into the job's Files slice, identifying which
	// file this article belongs to.
	FileIdx int

	// Offset is the byte position within the target file where Data should
	// be written. The caller (decoder) derives this from the article's
	// yBegin/yPart headers.
	Offset int64

	// Data is the decoded article payload. The assembler takes ownership;
	// callers must not modify Data after enqueueing.
	Data []byte
}

// FileInfo describes a target file. The assembler requests it from the caller's
// resolver the first time it encounters a (JobID, FileIdx) pair.
type FileInfo struct {
	// Path is the absolute target path, fully resolved and validated by
	// the caller's FileInfo resolver. The assembler trusts this value without
	// additional sandbox checks.
	Path string

	// TotalParts is the total number of WriteRequests expected for this file.
	// When the assembler has written TotalParts distinct requests, it closes
	// the file handle and fires OnFileComplete.
	//
	// Duplicate offsets: the assembler trusts the caller not to submit the same
	// offset twice. A duplicate increments the parts-written counter, causing it
	// to overshoot TotalParts and suppressing the completion callback. The queue
	// layer (Step 4.1) deduplicates via the article Done flag before enqueuing.
	TotalParts int
}

// Options configures an Assembler.
type Options struct {
	// QueueSize is the capacity of the internal write-request channel.
	// Zero selects the default (12, matching DEF_MAX_ASSEMBLER_QUEUE).
	QueueSize int

	// FileInfo is called once per (JobID, FileIdx) pair to obtain the target
	// path and expected part count. It must be non-nil; New panics otherwise.
	FileInfo func(jobID string, fileIdx int) (FileInfo, error)

	// OnFileComplete, if non-nil, is called on the worker goroutine when all
	// TotalParts for a file have been written and its handle has been closed.
	// The callback should be cheap; expensive work should be dispatched
	// asynchronously by the callback itself.
	OnFileComplete func(jobID string, fileIdx int)

	// OnLowDisk, if non-nil, is called when free space on the target
	// filesystem falls below MinFreeBytes. It is called on the worker goroutine
	// and should not block for long.
	OnLowDisk func(dir string, free int64)

	// MinFreeBytes is the low-disk threshold. Zero disables disk-space checks.
	MinFreeBytes int64
}

// fileKey uniquely identifies a target file within the assembler.
type fileKey struct {
	jobID   string
	fileIdx int
}

// openFile tracks an in-progress file being assembled.
type openFile struct {
	handle       *os.File
	info         FileInfo
	partsWritten int
}

// Assembler receives decoded article data and writes it to target files using
// WriteAt (pwrite on Unix). A single worker goroutine owns all file handles
// and performs all disk I/O, so no additional locking is needed for
// file-handle bookkeeping. WriteArticle blocks on the channel (backpressure)
// and is safe to call from multiple goroutines concurrently.
type Assembler struct {
	opts Options
	reqs chan WriteRequest

	// mu guards the started/stopped state and the stopCh channel.
	mu      sync.Mutex
	started bool
	stopped bool

	// stopCh is closed by Stop to signal the worker to begin draining.
	// We use a dedicated stop channel rather than closing reqs, because
	// closing reqs while WriteArticle goroutines may be sending on it would
	// cause a panic. The worker drains reqs after seeing stopCh is closed.
	stopCh     chan struct{}
	workerDone chan struct{}
}

// New creates an Assembler from opts. It panics if opts.FileInfo is nil.
func New(opts Options) *Assembler {
	if opts.FileInfo == nil {
		panic("assembler: Options.FileInfo must not be nil")
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	return &Assembler{
		opts:       opts,
		reqs:       make(chan WriteRequest, opts.QueueSize),
		stopCh:     make(chan struct{}),
		workerDone: make(chan struct{}),
	}
}

// Start launches the worker goroutine. It returns an error if called more than
// once without an intervening Stop.
func (a *Assembler) Start(_ context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.started {
		return errors.New("assembler: already started")
	}
	if a.stopped {
		return ErrStopped
	}

	a.started = true
	go a.worker()
	return nil
}

// Stop signals the worker to finish, drains any remaining requests, closes all
// open file handles, and blocks until the worker has exited. Partial files (not
// all TotalParts written) are closed without firing OnFileComplete.
//
// Stop is safe to call before Start (no-op) and safe to call multiple times
// (second call is a no-op).
func (a *Assembler) Stop() error {
	a.mu.Lock()

	if !a.started || a.stopped {
		a.mu.Unlock()
		return nil
	}
	a.stopped = true
	// Signal the worker by closing stopCh. The worker will drain reqs and exit.
	// We do NOT close reqs here: WriteArticle goroutines may still be executing
	// their channel-send select, and closing a channel that has concurrent
	// senders in flight causes a panic.
	close(a.stopCh)
	a.mu.Unlock()

	<-a.workerDone
	return nil
}

// WriteArticle enqueues req for writing. It blocks until the worker accepts the
// request or ctx is cancelled. Returns ErrStopped if Stop has been called.
// Returns ctx.Err() if ctx is cancelled while waiting for channel capacity.
func (a *Assembler) WriteArticle(ctx context.Context, req WriteRequest) error {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return ErrNotStarted
	}
	if a.stopped {
		a.mu.Unlock()
		return ErrStopped
	}
	a.mu.Unlock()

	select {
	case a.reqs <- req:
		return nil
	case <-a.stopCh:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// worker is the single goroutine that owns all file handles and performs disk
// I/O. It runs until stopCh is closed and the request channel is drained.
//
// No batching or flush timer: unlike the Python assembler, which uses a 5-second
// write-interval timer as a handoff optimization between articlecache and
// assembler, Go's os.File.WriteAt issues a single pwrite syscall per call.
// Per-article writes are already optimal — there is nothing to batch.
func (a *Assembler) worker() {
	defer close(a.workerDone)

	open := make(map[fileKey]*openFile)
	reqCount := 0

	for {
		select {
		case req, ok := <-a.reqs:
			if !ok {
				// Channel was closed; this path is not taken in normal operation
				// (we never close reqs), but defend against it.
				a.closeAll(open)
				return
			}
			a.processRequest(req, open)
			reqCount++
			if a.opts.MinFreeBytes > 0 && reqCount%diskCheckInterval == 0 {
				a.checkDiskSpace(open)
			}

		case <-a.stopCh:
			// Drain any requests that were already in the channel before stopCh
			// was closed. New WriteArticle calls see stopCh and return ErrStopped,
			// so the channel will not receive new items after this point.
			for {
				select {
				case req := <-a.reqs:
					a.processRequest(req, open)
					reqCount++
					if a.opts.MinFreeBytes > 0 && reqCount%diskCheckInterval == 0 {
						a.checkDiskSpace(open)
					}
				default:
					// Channel drained.
					a.closeAll(open)
					return
				}
			}
		}
	}
}

// closeAll closes all remaining open file handles. Called on worker exit.
// Completion callbacks do NOT fire for partial files — writing N-of-M parts
// is not a completion event.
func (a *Assembler) closeAll(open map[fileKey]*openFile) {
	for _, f := range open {
		if err := f.handle.Close(); err != nil {
			slog.Warn("assembler: close partial file on shutdown",
				"path", f.info.Path,
				"error", err,
			)
		}
	}
}

// processRequest performs the WriteAt for a single WriteRequest. It resolves
// the target file on first encounter, caches the handle, and fires
// OnFileComplete when all TotalParts have been written.
func (a *Assembler) processRequest(req WriteRequest, open map[fileKey]*openFile) {
	key := fileKey{jobID: req.JobID, fileIdx: req.FileIdx}

	f, ok := open[key]
	if !ok {
		info, err := a.opts.FileInfo(req.JobID, req.FileIdx)
		if err != nil {
			slog.Warn("assembler: FileInfo resolver failed; discarding article",
				"jobID", req.JobID,
				"fileIdx", req.FileIdx,
				"error", err,
			)
			return
		}

		//nolint:gosec // G304: path is caller-supplied from FileInfo resolver, which is responsible for safe derivation
		fh, err := os.OpenFile(info.Path, os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			slog.Error("assembler: open target file",
				"path", info.Path,
				"error", err,
			)
			return
		}
		f = &openFile{handle: fh, info: info}
		open[key] = f
	}

	if err := writeAll(f.handle, req.Data, req.Offset); err != nil {
		slog.Error("assembler: write article",
			"path", f.info.Path,
			"offset", req.Offset,
			"error", err,
		)
		// Leave the file open; the next article may succeed. The pipeline
		// (Step 4.1) is responsible for job-level failure detection.
		return
	}

	f.partsWritten++
	if f.info.TotalParts > 0 && f.partsWritten >= f.info.TotalParts {
		if err := f.handle.Close(); err != nil {
			slog.Warn("assembler: close completed file",
				"path", f.info.Path,
				"error", err,
			)
		}
		delete(open, key)
		if a.opts.OnFileComplete != nil {
			a.opts.OnFileComplete(req.JobID, req.FileIdx)
		}
	}
}

// checkDiskSpace queries free space on each unique directory currently
// holding open files and calls OnLowDisk when free < MinFreeBytes.
func (a *Assembler) checkDiskSpace(open map[fileKey]*openFile) {
	if a.opts.OnLowDisk == nil {
		return
	}
	// Collect unique directories to avoid redundant syscalls when many files
	// share the same directory (the common case).
	seen := make(map[string]struct{}, len(open))
	for _, f := range open {
		dir := parentDir(f.info.Path)
		if _, already := seen[dir]; already {
			continue
		}
		seen[dir] = struct{}{}

		free, err := FreeBytes(dir)
		if err != nil {
			slog.Warn("assembler: disk-space check failed", "dir", dir, "error", err)
			continue
		}
		if free < a.opts.MinFreeBytes {
			a.opts.OnLowDisk(dir, free)
		}
	}
}

// writeAll writes all of data to f at offset, retrying partial writes.
// os.File.WriteAt does not guarantee a full write in a single call (analogous
// to Python's non-buffered os.write needing a loop for partial writes).
func writeAll(f *os.File, data []byte, offset int64) error {
	written := 0
	for written < len(data) {
		n, err := f.WriteAt(data[written:], offset+int64(written))
		if err != nil {
			return fmt.Errorf("writeAt offset %d: %w", offset+int64(written), err)
		}
		written += n
	}
	return nil
}

// parentDir returns the directory portion of path. Equivalent to
// filepath.Dir but avoids importing path/filepath for this single use.
func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}
