package assembler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// defaultQueueSize mirrors DEF_MAX_ASSEMBLER_QUEUE from the Python source.
const defaultQueueSize = 12

// defaultDoneFlushInterval is how often the worker flushes its pending
// Done/Failed batches to the queue when no OnFileComplete has fired
// recently. Short enough to keep UI progress lively on long-running
// files; long enough to collapse bursts of small-article completions
// into a single queue lock acquisition. 250ms is comfortably below
// human reaction time for progress updates.
const defaultDoneFlushInterval = 250 * time.Millisecond

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

	// MessageID is the article's NNTP Message-ID. The assembler uses it to
	// mark the article Done (on success, after fsync) or Failed (on FatalErr)
	// in the queue. Required.
	MessageID string

	// Offset is the byte position within the target file where Data should
	// be written. The caller (decoder) derives this from the article's
	// yBegin/yPart headers.
	Offset int64

	// Data is the decoded article payload. The assembler takes ownership;
	// callers must not modify Data after enqueueing.
	Data []byte

	// FatalErr is set if the article permanently failed to download.
	// If non-nil, the assembler skips writing and counts the part toward
	// file completion. Duplicate failures are deduplicated locally
	// (per-file seen-set) so partsWritten does not overshoot TotalParts.
	FatalErr error
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

	// MarkArticlesDone is the batched durability callback: the assembler
	// accumulates message-IDs of successfully fsynced articles and hands
	// them to this function in groups, either when a file completes, on
	// a periodic flush timer (DoneFlushInterval), or at Stop. Taking the
	// queue write lock once per batch (instead of once per article) is
	// the whole point — a completions firehose would otherwise serialise
	// against every RLock-reader. Required when articles must survive a
	// crash.
	MarkArticlesDone func(jobID string, messageIDs []string) error

	// MarkArticlesFailed is the batched form of the FatalErr callback.
	// The returned firstTimeIDs slice is informational (consumers that
	// care about first-time failure events can use it); the assembler
	// itself dedupes locally via a per-file seen-set, so partsWritten
	// tracking does not depend on this return value.
	MarkArticlesFailed func(jobID string, messageIDs []string) (firstTimeIDs []string, err error)

	// DoneFlushInterval overrides the default 250ms flush cadence for
	// pending Done/Failed batches. Zero selects the default; negative
	// disables the timer (flush only on file completion or Stop — useful
	// for benchmarks that want to measure pure batching behaviour).
	DoneFlushInterval time.Duration
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
	// seenFailed dedupes FatalErr requests by Message-ID so a duplicate
	// emission (shouldn't happen under B.6's Emitted gate, but defence
	// in depth) cannot double-count a part toward TotalParts.
	seenFailed map[string]struct{}
}

// Assembler receives decoded article data and writes it to target files using
// WriteAt (pwrite on Unix). A single worker goroutine owns all file handles
// and performs all disk I/O, so no additional locking is needed for
// file-handle bookkeeping. WriteArticle blocks on the channel (backpressure)
// and is safe to call from multiple goroutines concurrently.
type Assembler struct {
	log  *slog.Logger
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

	// flushInterval is the computed interval for the periodic batch
	// flush. A non-positive value disables the timer entirely (flush
	// only on file completion or Stop).
	flushInterval time.Duration

	// pendingDone and pendingFailed are per-job batches accumulated by
	// the worker goroutine between flushes. Exclusively owned by the
	// worker — no locking.
	pendingDone   map[string][]string
	pendingFailed map[string][]string
}

// New creates an Assembler from opts. It panics if opts.FileInfo is nil.
func New(opts Options, log *slog.Logger) *Assembler {
	if opts.FileInfo == nil {
		panic("assembler: Options.FileInfo must not be nil")
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	if log == nil {
		log = slog.Default()
	}
	flushInterval := opts.DoneFlushInterval
	if flushInterval == 0 {
		flushInterval = defaultDoneFlushInterval
	}
	return &Assembler{
		log:           log.With("component", "assembler"),
		opts:          opts,
		reqs:          make(chan WriteRequest, opts.QueueSize),
		stopCh:        make(chan struct{}),
		workerDone:    make(chan struct{}),
		flushInterval: flushInterval,
		pendingDone:   make(map[string][]string),
		pendingFailed: make(map[string][]string),
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
// Batching: successful fsyncs and FatalErr accounting are collected into
// pendingDone / pendingFailed maps and flushed to the queue in groups —
// either when a file completes (inside processRequest, before the
// OnFileComplete callback), when the flush ticker fires, or at Stop.
// The per-article pwrite + fsync remains serial; only the queue-mutation
// path is batched. See B.7 in docs/state_machine_hardening_plan.md.
func (a *Assembler) worker() {
	defer close(a.workerDone)

	open := make(map[fileKey]*openFile)
	reqCount := 0

	// A nil channel blocks forever in select; that's how we disable the
	// flush timer when DoneFlushInterval < 0 (benchmark mode).
	var tickC <-chan time.Time
	if a.flushInterval > 0 {
		t := time.NewTicker(a.flushInterval)
		defer t.Stop()
		tickC = t.C
	}

	for {
		select {
		case req, ok := <-a.reqs:
			if !ok {
				// Channel was closed; this path is not taken in normal operation
				// (we never close reqs), but defend against it.
				a.flush()
				a.closeAll(open)
				return
			}
			a.processRequest(req, open)
			reqCount++
			if a.opts.MinFreeBytes > 0 && reqCount%diskCheckInterval == 0 {
				a.checkDiskSpace(open)
			}

		case <-tickC:
			a.flush()

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
					// Channel drained. Final flush before closing files so the
					// queue sees every Done/Failed that made it to disk.
					a.flush()
					a.closeAll(open)
					return
				}
			}
		}
	}
}

// flush drains the pending Done and Failed batches to the queue. Called
// on the worker goroutine (no locking on a.pending*). Errors are logged
// and swallowed: the queue mutation is best-effort once bytes are on
// disk, and the next completion will retry implicitly (partsWritten
// tracking is local to the assembler).
func (a *Assembler) flush() {
	if len(a.pendingDone) == 0 && len(a.pendingFailed) == 0 {
		return
	}
	if a.opts.MarkArticlesDone != nil {
		for jobID, msgIDs := range a.pendingDone {
			if err := a.opts.MarkArticlesDone(jobID, msgIDs); err != nil {
				a.log.Warn("batch mark articles done",
					"job", jobID, "count", len(msgIDs), "error", err)
			}
		}
	}
	if a.opts.MarkArticlesFailed != nil {
		for jobID, msgIDs := range a.pendingFailed {
			if _, err := a.opts.MarkArticlesFailed(jobID, msgIDs); err != nil {
				a.log.Warn("batch mark articles failed",
					"job", jobID, "count", len(msgIDs), "error", err)
			}
		}
	}
	// Reset the maps. Reuse the backing allocation where reasonable by
	// clearing rather than reallocating (Go 1.21+ `clear` semantics).
	clear(a.pendingDone)
	clear(a.pendingFailed)
}

// closeAll closes all remaining open file handles. Called on worker exit.
// Completion callbacks do NOT fire for partial files — writing N-of-M parts
// is not a completion event.
func (a *Assembler) closeAll(open map[fileKey]*openFile) {
	for _, f := range open {
		if err := f.handle.Close(); err != nil {
			a.log.Warn("close partial file on shutdown",
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
			a.log.Warn("FileInfo resolver failed; discarding article",
				"jobID", req.JobID,
				"fileIdx", req.FileIdx,
				"error", err,
			)
			return
		}

		if dir := filepath.Dir(info.Path); dir != "" {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				a.log.Error("mkdir parent",
					"path", info.Path,
					"error", err,
				)
				return
			}
		}
		//nolint:gosec // G304: path is caller-supplied from FileInfo resolver, which is responsible for safe derivation
		fh, err := os.OpenFile(info.Path, os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			a.log.Error("open target file",
				"path", info.Path,
				"error", err,
			)
			return
		}
		f = &openFile{handle: fh, info: info}
		open[key] = f
	}

	if req.FatalErr != nil {
		a.log.Info("counting failed article toward completion (skipping disk write)",
			"job", req.JobID, "fileidx", req.FileIdx, "path", f.info.Path, "error", req.FatalErr)
		if f.seenFailed == nil {
			f.seenFailed = make(map[string]struct{})
		}
		if _, dup := f.seenFailed[req.MessageID]; dup {
			// Duplicate failure — don't double-count toward completion.
			return
		}
		f.seenFailed[req.MessageID] = struct{}{}
		a.pendingFailed[req.JobID] = append(a.pendingFailed[req.JobID], req.MessageID)
	} else {
		if err := writeAll(f.handle, req.Data, req.Offset); err != nil {
			a.log.Error("write article",
				"path", f.info.Path,
				"offset", req.Offset,
				"error", err,
			)
			// Leave the file open; the next article may succeed. The pipeline
			// (Step 4.1) is responsible for job-level failure detection.
			return
		}
		// Durability: fsync the fd before recording Done. If fsync fails
		// we must not record Done — a later restart would see Done=true
		// with no bytes on disk.
		if err := f.handle.Sync(); err != nil {
			a.log.Error("fsync article",
				"path", f.info.Path, "offset", req.Offset, "error", err)
			return
		}
		a.pendingDone[req.JobID] = append(a.pendingDone[req.JobID], req.MessageID)
	}

	f.partsWritten++
	a.log.Debug("processed part",
		"job", req.JobID, "fileidx", req.FileIdx,
		"part", f.partsWritten, "total", f.info.TotalParts,
		"offset", req.Offset, "bytes", len(req.Data), "failed", req.FatalErr != nil)
	if f.info.TotalParts > 0 && f.partsWritten >= f.info.TotalParts {
		if err := f.handle.Close(); err != nil {
			a.log.Warn("close completed file",
				"path", f.info.Path,
				"error", err,
			)
		}
		delete(open, key)
		a.log.Info("file complete", "job", req.JobID, "fileidx", req.FileIdx, "path", f.info.Path)
		// Flush pending Done/Failed before firing the callback. The
		// pipeline's watchCompletions must not observe IsComplete()==true
		// on a file whose articles are not yet marked Done in the queue,
		// or it will race job-completion logic ahead of durability state.
		a.flush()
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
			a.log.Warn("disk-space check failed", "dir", dir, "error", err)
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
