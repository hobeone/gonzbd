package assembler

import (
	"cmp"
	"context"

	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/internal/decoder"

	"github.com/hobeone/gonzbd/internal/crc32util"
	"github.com/hobeone/gonzbd/internal/telemetry"
)

// defaultQueueSize is the capacity of the internal write-request channel.
// Increased from the Python source's 12 to 2048 to absorb disk I/O spikes
// on high-speed (1Gbps+) connections.
const defaultQueueSize = 2048

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

// diskCheckTimeout bounds each per-directory FreeBytes call in checkDiskSpace.
// statfs can block uninterruptibly on a stuck network mount (NFS/SMB); without
// this bound the single assembler worker — which owns all open file handles
// and is the only drainer of a.reqs — stalls the whole pipeline for as long as
// the mount stays wedged.
const diskCheckTimeout = 5 * time.Second

var (
	// ErrNotStarted is returned by WriteArticle when Start has not yet been called.
	ErrNotStarted = errors.New("assembler: not started")

	// ErrStopped is returned by WriteArticle after Stop has been called.
	ErrStopped = errors.New("assembler: stopped")
)

// WriteRequest is the unit of work sent to the assembler. Each request
// corresponds to one decoded NZB article segment.
type WriteRequest struct {
	// JobID identifies the parent download job.
	JobID string

	// FileIdx is the index into the job's Files slice, identifying which
	// file this article belongs to.
	FileIdx int

	// ArtIdx is the global index of the article within the job's manifest.
	ArtIdx int32

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

	// CRC is the CRC32 of the decoded article data. Used to incrementally
	// build a per-file CRC via crc32util.Combine for QuickCheck
	// verification against par2 file hashes. Zero when the article failed
	// or was UU-encoded.
	CRC uint32

	// ackCh, when non-nil, is closed by the worker immediately after this
	// control message (cancel) has been fully processed -- i.e. after the
	// job's open file handles have been closed and removed. Set only by
	// CancelJob; never used by ordinary write requests.
	ackCh chan struct{}
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

	// ExpectedSize is the NZB's claimed total decoded size for this file
	// (sum of article byte counts). When positive, the assembler
	// pre-allocates the file at this size on first open, reducing
	// per-write filesystem metadata overhead and fragmentation.
	// Zero disables pre-allocation.
	ExpectedSize int64

	// InitialWriteCursor seeds the write-coalescing cache's per-file cursor.
	// Zero for a fresh download (file starts at byte 0). On resume the caller
	// sets it to the file's persisted contiguous write frontier so coalescing
	// doesn't stall waiting for an offset-0 article that was already written
	// before the restart. See queue.JobFile.WriteCursor.
	InitialWriteCursor int64
}

// Options configures an Assembler.
type Options struct {
	// QueueSize is the capacity of the internal write-request channel.
	// Zero selects the default (2048).
	QueueSize int

	// FileInfo is called once per (JobID, FileIdx) pair to obtain the target
	// path and expected part count. It must be non-nil; New panics otherwise.
	FileInfo func(jobID string, fileIdx int) (FileInfo, error)

	// OnFileComplete, if non-nil, is called on the worker goroutine when all
	// TotalParts for a file have been written and its handle has been closed.
	// fileCRC is the CRC32 of the complete file, computed by combining
	// per-article CRCs in offset order. It is zero if any articles lacked
	// CRC information (e.g. UU-encoded or failed). The callback should be
	// cheap; expensive work should be dispatched asynchronously.
	OnFileComplete func(jobID string, fileIdx int, fileCRC uint32)

	// OnLowDisk, if non-nil, is called when free space on the target
	// filesystem falls below MinFreeBytes. It is called on the worker goroutine
	// and should not block for long.
	OnLowDisk func(dir string, free int64)

	// MinFreeBytes is the low-disk threshold. Zero disables disk-space checks.
	MinFreeBytes int64

	// MarkArticlesDoneByIdx is the O(1) index-based batched durability callback.
	MarkArticlesDoneByIdx func(jobID string, artIdxs []int32) error

	// MarkArticlesFailedByIdx is the O(1) index-based batched failure callback.
	MarkArticlesFailedByIdx func(jobID string, artIdxs []int32) ([]int32, error)

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

	// WriteCacheBytes is the memory limit for the write coalescing cache.
	// When positive, decoded articles are buffered in memory and flushed
	// as larger contiguous writes, reducing syscall count and improving
	// sequential write patterns. Zero disables caching (each article is
	// written individually, which is the pre-5.0 behavior).
	WriteCacheBytes int64

	// SetWriteCursor persists a file's advanced contiguous write frontier as a
	// resume hint. Called from the worker's batched flush, never per-article.
	// Optional; nil disables cursor persistence.
	SetWriteCursor func(jobID string, fileIdx int, cursor int64) error
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
	// maxWritten tracks the highest byte position written (offset + len).
	// Used to truncate the file to its true decoded size at completion.
	// Pre-allocation (fallocate/ftruncate) sets the file size to the
	// NZB-declared encoded size, which is ~2% larger than the actual
	// decoded content. Without truncation, the trailing zeros cause
	// par2 to report files as damaged despite 100% download health.
	maxWritten int64
	// seenFailed dedupes FatalErr requests by Message-ID so a duplicate
	// emission (shouldn't happen under B.6's Emitted gate, but defence
	// in depth) cannot double-count a part toward TotalParts.
	seenFailed map[string]struct{}
	// seenDone dedupes successful writes symmetrically with seenFailed.
	seenDone map[string]struct{}
	// crcParts accumulates per-article CRC32 values with their offsets.
	// At file completion, these are sorted by offset and combined using
	// crc32util.Combine to produce the whole-file CRC32.
	crcParts []crcPart
	// crcValid tracks whether all articles had valid CRC values.
	// If any article had CRC=0 (UU-encoded or failed), this is set
	// to false and the final file CRC is reported as 0.
	crcValid bool
}

// crcPart stores the CRC32 of a single article part along with its
// byte offset and decoded length. Used to reconstruct the whole-file
// CRC at completion time by combining parts in offset order.
type crcPart struct {
	offset int64
	crc    uint32
	len    int64
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

	// minFreeBytes is the hot-changeable disk-space threshold. It shadows
	// opts.MinFreeBytes and is set atomically via SetMinFreeBytes so config
	// saves from the API goroutine don't race with the worker's disk checks.
	minFreeBytes atomic.Int64

	// diskProbe bounds checkDiskSpace's statfs calls to at most one
	// outstanding probe per directory, with a short TTL cache, so a stuck
	// NFS/SMB mount leaks at most one goroutine instead of one per
	// diskCheckInterval writes for as long as the mount stays down. Tests
	// override its statfs field (same-package, set once before
	// Start/checkDiskSpace is ever invoked on this instance) to simulate a
	// hung statfs without a real dead mount.
	diskProbe *DiskProbe

	// cacheUsedBytes mirrors writeCache.used so it can be read safely from
	// goroutines other than the worker goroutine (writeCache itself is
	// documented as single-goroutine, no-lock). Updated after every
	// dispatchRequest call (via defer, covering processRequest and
	// wc.forget on job cancel) and again after the shutdown drain
	// (flushWriteCache in worker()), so it stays accurate through the
	// only two places writeCache.used can change.
	cacheUsedBytes atomic.Int64

	// mu guards the started/stopped state and the stopCh channel.
	mu      sync.Mutex
	started bool
	stopped bool

	// stopCh is closed by Stop to signal the worker to begin draining.
	// We use a dedicated stop channel rather than closing reqs, because
	// closing reqs while WriteArticle goroutines may be sending on it would
	// cause a panic. The worker drains reqs after seeing stopCh is closed.
	stopCh chan struct{}

	// wg tracks all in-flight WriteArticle and CancelJob calls as well as
	// the worker goroutine, ensuring Stop() blocks cleanly without sleep
	// polling until all in-flight work and the worker have finished.
	wg sync.WaitGroup

	// flushInterval is the computed interval for the periodic batch
	// flush. A non-positive value disables the timer entirely (flush
	// only on file completion or Stop).
	flushInterval time.Duration

	// pendingDone and pendingFailed are per-job batches accumulated by
	// the worker goroutine between flushes. Exclusively owned by the
	// worker — no locking.
	pendingDone        map[string][]string
	pendingFailed      map[string][]string
	pendingDoneByIdx   map[string][]int32
	pendingFailedByIdx map[string][]int32

	// pendingCursor holds the latest reported write cursor per file, flushed
	// to Options.SetWriteCursor on the same cadence as pendingDone. Worker-owned.
	pendingCursor map[fileKey]int64
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
	a := &Assembler{
		log:                log.With("component", "assembler"),
		opts:               opts,
		reqs:               make(chan WriteRequest, opts.QueueSize),
		stopCh:             make(chan struct{}),
		flushInterval:      flushInterval,
		pendingDone:        make(map[string][]string),
		pendingFailed:      make(map[string][]string),
		pendingDoneByIdx:   make(map[string][]int32),
		pendingFailedByIdx: make(map[string][]int32),
		pendingCursor:      make(map[fileKey]int64),
		diskProbe:          NewDiskProbe(DefaultDiskProbeTTL),
	}
	a.minFreeBytes.Store(opts.MinFreeBytes)
	return a
}

// SetMinFreeBytes updates the low-disk threshold without restarting the
// assembler. Zero disables disk-space checks. Thread-safe.
func (a *Assembler) SetMinFreeBytes(v int64) { a.minFreeBytes.Store(v) }

// MinFreeBytes returns the current low-disk threshold in bytes. Thread-safe.
func (a *Assembler) MinFreeBytes() int64 { return a.minFreeBytes.Load() } //nocover: trivial atomic load

// CacheUsageBytes returns the current number of bytes buffered in the
// write-coalescing cache. Safe to call from any goroutine.
func (a *Assembler) CacheUsageBytes() int64 {
	return a.cacheUsedBytes.Load()
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
	a.wg.Add(1)
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
	a.mu.Unlock()

	// Close stopCh first to unblock any WriteArticle goroutines stuck in
	// their select (e.g. waiting for channel capacity). They will see
	// <-a.stopCh and return ErrStopped, calling wg.Done(). If the
	// request channel has capacity, their send may also succeed — the
	// worker will drain those items before exiting.
	close(a.stopCh)

	// Wait for all in-flight WriteArticle/CancelJob goroutines and the worker
	// goroutine to finish cleanly without sleep polling.
	a.wg.Wait()
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
	// Track this sender so Stop() waits for us before returning.
	a.wg.Add(1)
	a.mu.Unlock()

	defer a.wg.Done()

	select {
	case a.reqs <- req:
		return nil
	case <-a.stopCh:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CancelJob sends a control message to the worker goroutine to close all
// open file handles for the given job, and blocks until the worker has
// actually done so. This prevents FD leaks when a job is removed from the
// queue while articles are still being assembled, and lets callers safely
// delete the job's directory immediately after CancelJob returns without
// racing the worker's Close()+Remove() of files still inside it (which on
// NFS-mounted directories produces .nfsXXXXXX silly-rename artifacts and a
// directory the caller's delete can't remove).
func (a *Assembler) CancelJob(ctx context.Context, jobID string) error {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return ErrNotStarted
	}
	if a.stopped {
		a.mu.Unlock()
		return ErrStopped
	}
	a.wg.Add(1)
	a.mu.Unlock()

	defer a.wg.Done()

	// Control message convention: JobID="" and FileIdx=-1, with the
	// real job ID in MessageID.
	ack := make(chan struct{})
	control := WriteRequest{
		JobID:     "",
		FileIdx:   -1,
		MessageID: jobID,
		ackCh:     ack,
	}
	select {
	case a.reqs <- control:
	case <-a.stopCh:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}

	// Wait for the worker to actually process the control message. If Stop
	// races and drains it during shutdown, the worker still closes ack (see
	// dispatchRequest), so this is never left blocking indefinitely.
	select {
	case <-ack:
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
	defer a.wg.Done()

	open := make(map[fileKey]*openFile)
	completed := make(map[fileKey]struct{})    // tombstone set for finished files
	cancelledJobs := make(map[string]struct{}) // tombstone set for cancelled jobs
	reqCount := 0
	wc := newWriteCache(a.opts.WriteCacheBytes)

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
			reqCount += a.dispatchRequest(req, open, completed, cancelledJobs, wc)
			if a.minFreeBytes.Load() > 0 && reqCount%diskCheckInterval == 0 {
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
					reqCount += a.dispatchRequest(req, open, completed, cancelledJobs, wc)
					if a.minFreeBytes.Load() > 0 && reqCount%diskCheckInterval == 0 {
						a.checkDiskSpace(open)
					}
				default:
					// Drain all cached articles to disk before shutdown.
					a.flushWriteCache(wc, open)
					a.cacheUsedBytes.Store(wc.used)
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

// dispatchRequest handles a single request from the channel. It processes
// cancel control messages (JobID="" && FileIdx==-1) by closing and removing
// all open files for the cancelled job, skips articles for already-cancelled
// jobs, and delegates normal write requests to processRequest. Returns 1 if
// a normal request was processed (for reqCount tracking), 0 otherwise.
//
// This method is called from both the main select loop and the shutdown
// drain loop to ensure cancel messages are handled correctly in both paths.
func (a *Assembler) dispatchRequest(
	req WriteRequest,
	open map[fileKey]*openFile,
	completed map[fileKey]struct{},
	cancelledJobs map[string]struct{},
	wc *writeCache,
) int {
	defer func() { a.cacheUsedBytes.Store(wc.used) }()

	if req.JobID == "" && req.FileIdx == -1 {
		// Control message: cancel a job. Close and remove all
		// open files for the job encoded in MessageID.
		cancelID := req.MessageID
		cancelledJobs[cancelID] = struct{}{}
		for k, f := range open {
			if k.jobID != cancelID {
				continue
			}
			_ = f.handle.Close() //nolint:errcheck // best-effort; file is immediately removed
			if err := os.Remove(f.info.Path); err != nil && !os.IsNotExist(err) {
				a.log.Warn("failed to remove cancelled file",
					"path", f.info.Path, "error", err)
			}
			delete(open, k)
			completed[k] = struct{}{}
			wc.forget(k) // discard cached articles for cancelled file
		}
		if req.ackCh != nil {
			close(req.ackCh)
		}
		return 0
	}
	// Skip articles for cancelled jobs.
	if _, cancelled := cancelledJobs[req.JobID]; cancelled {
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
		return 0
	}
	a.processRequest(req, open, completed, wc)
	return 1
}

// flush drains the pending Done and Failed batches to the queue. Called
// on the worker goroutine (no locking on a.pending*). Errors are logged
// and swallowed: the queue mutation is best-effort once bytes are on
// disk, and the next completion will retry implicitly (partsWritten
// tracking is local to the assembler).
func (a *Assembler) flush() {
	if len(a.pendingDoneByIdx) == 0 && len(a.pendingFailedByIdx) == 0 &&
		len(a.pendingDone) == 0 && len(a.pendingFailed) == 0 && len(a.pendingCursor) == 0 {
		return
	}
	if a.opts.MarkArticlesDoneByIdx != nil {
		for jobID, artIdxs := range a.pendingDoneByIdx {
			if err := a.opts.MarkArticlesDoneByIdx(jobID, artIdxs); err != nil {
				a.log.Debug("batch mark articles done by idx (job already removed)",
					"job", jobID, "count", len(artIdxs), "error", err)
			}
		}
	} else if a.opts.MarkArticlesDone != nil {
		for jobID, msgIDs := range a.pendingDone {
			if err := a.opts.MarkArticlesDone(jobID, msgIDs); err != nil {
				a.log.Debug("batch mark articles done (job already removed)",
					"job", jobID, "count", len(msgIDs), "error", err)
			}
		}
	}

	if a.opts.MarkArticlesFailedByIdx != nil {
		for jobID, artIdxs := range a.pendingFailedByIdx {
			if _, err := a.opts.MarkArticlesFailedByIdx(jobID, artIdxs); err != nil {
				a.log.Debug("batch mark articles failed by idx (job already removed)",
					"job", jobID, "count", len(artIdxs), "error", err)
			}
		}
	} else if a.opts.MarkArticlesFailed != nil {
		for jobID, msgIDs := range a.pendingFailed {
			if _, err := a.opts.MarkArticlesFailed(jobID, msgIDs); err != nil {
				a.log.Debug("batch mark articles failed (job already removed)",
					"job", jobID, "count", len(msgIDs), "error", err)
			}
		}
	}

	if a.opts.SetWriteCursor != nil {
		for k, cur := range a.pendingCursor {
			if err := a.opts.SetWriteCursor(k.jobID, k.fileIdx, cur); err != nil {
				a.log.Debug("set write cursor (job already removed)",
					"job", k.jobID, "fileidx", k.fileIdx, "error", err)
			}
		}
	}
	clear(a.pendingDone)
	clear(a.pendingFailed)
	clear(a.pendingDoneByIdx)
	clear(a.pendingFailedByIdx)
	clear(a.pendingCursor)
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
// OnFileComplete when all TotalParts have been written. When write coalescing
// is enabled (wc.enabled()), articles are buffered in memory and flushed as
// larger contiguous writes; otherwise each article is written individually.
func (a *Assembler) processRequest(req WriteRequest, open map[fileKey]*openFile, completed map[fileKey]struct{}, wc *writeCache) {
	key := fileKey{jobID: req.JobID, fileIdx: req.FileIdx}

	if _, done := completed[key]; done {
		a.handleLateDuplicate(req)
		return
	}

	f, ok := open[key]
	if !ok {
		f = a.openTargetFile(key, req, open, wc)
		if f == nil {
			return
		}
	}

	if req.FatalErr != nil {
		if !a.handleFatalArticle(f, req) {
			return
		}
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
	} else if !a.handleSuccessArticle(f, req, wc, open, key) {
		return
	}

	f.partsWritten++
	a.log.Debug("processed part",
		"job", req.JobID, "fileidx", req.FileIdx,
		"part", f.partsWritten, "total", f.info.TotalParts,
		"offset", req.Offset, "bytes", len(req.Data), "failed", req.FatalErr != nil)
	if f.info.TotalParts > 0 && f.partsWritten >= f.info.TotalParts {
		a.finalizeFile(f, key, req, open, completed, wc)
	}
}

func (a *Assembler) recordPendingDone(jobID, msgID string, artIdx int32) {
	if a.opts.MarkArticlesDoneByIdx != nil {
		if a.pendingDoneByIdx == nil {
			a.pendingDoneByIdx = make(map[string][]int32)
		}
		a.pendingDoneByIdx[jobID] = append(a.pendingDoneByIdx[jobID], artIdx)
	} else if a.opts.MarkArticlesDone != nil {
		if a.pendingDone == nil {
			a.pendingDone = make(map[string][]string)
		}
		a.pendingDone[jobID] = append(a.pendingDone[jobID], msgID)
	}
}

func (a *Assembler) recordPendingFailed(jobID, msgID string, artIdx int32) {
	if a.opts.MarkArticlesFailedByIdx != nil {
		if a.pendingFailedByIdx == nil {
			a.pendingFailedByIdx = make(map[string][]int32)
		}
		a.pendingFailedByIdx[jobID] = append(a.pendingFailedByIdx[jobID], artIdx)
	} else if a.opts.MarkArticlesFailed != nil {
		if a.pendingFailed == nil {
			a.pendingFailed = make(map[string][]string)
		}
		a.pendingFailed[jobID] = append(a.pendingFailed[jobID], msgID)
	}
}

// handleLateDuplicate handles articles arriving for a file that is already marked completed.
func (a *Assembler) handleLateDuplicate(req WriteRequest) {
	a.log.Debug("ignoring late article for completed file",
		"job", req.JobID, "fileidx", req.FileIdx, "msgid", req.MessageID)
	if req.FatalErr != nil {
		a.recordPendingFailed(req.JobID, req.MessageID, req.ArtIdx)
	} else {
		a.recordPendingDone(req.JobID, req.MessageID, req.ArtIdx)
	}
	if req.Data != nil {
		decoder.PutBuffer(req.Data)
	}
}

// openTargetFile resolves file information and creates the target file on disk for a new fileKey.
func (a *Assembler) openTargetFile(key fileKey, req WriteRequest, open map[fileKey]*openFile, wc *writeCache) *openFile {
	info, err := a.opts.FileInfo(req.JobID, req.FileIdx)
	if err != nil {
		a.log.Warn("FileInfo resolver failed; discarding article",
			"jobID", req.JobID,
			"fileIdx", req.FileIdx,
			"error", err,
		)
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
		return nil
	}

	dir := filepath.Dir(info.Path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		a.log.Error("mkdir parent",
			"path", info.Path,
			"error", err,
		)
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
		return nil
	}
	//nolint:gosec // G304: path is caller-supplied from FileInfo resolver, which is responsible for safe derivation
	fh, err := os.OpenFile(info.Path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		a.log.Error("open target file",
			"path", info.Path,
			"error", err,
		)
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
		return nil
	}
	if info.ExpectedSize > 0 {
		telemetry.PreallocCalls.Add(1)
		if err := preallocateFile(fh, info.ExpectedSize); err != nil {
			a.log.Warn("file pre-allocation failed, continuing without",
				"path", info.Path,
				"size", info.ExpectedSize,
				"error", err,
			)
		}
	} else {
		a.log.Debug("zero-length expected size for target file",
			"path", info.Path,
		)
	}
	f := &openFile{handle: fh, info: info, crcValid: true}
	open[key] = f
	wc.initCursor(key, info.InitialWriteCursor)
	return f
}

func (a *Assembler) handleFatalArticle(f *openFile, req WriteRequest) bool {
	f.crcValid = false // Failed articles invalidate CRC tracking
	a.log.Debug("counting failed article toward completion (skipping disk write)",
		"job", req.JobID, "fileidx", req.FileIdx, "path", f.info.Path, "error", req.FatalErr)
	if f.seenFailed == nil {
		f.seenFailed = make(map[string]struct{})
	}
	if _, dup := f.seenFailed[req.MessageID]; dup {
		// Already recorded — re-emit the ack so the queue
		// receives it even if a prior flush dropped it.
		a.recordPendingFailed(req.JobID, req.MessageID, req.ArtIdx)
		return false
	}
	// Cross-check: if this MessageID was already counted as a success,
	// don't increment partsWritten again — just record the failure ack.
	alreadyCounted := false
	if f.seenDone != nil {
		if _, was := f.seenDone[req.MessageID]; was {
			alreadyCounted = true
		}
	}
	f.seenFailed[req.MessageID] = struct{}{}
	a.recordPendingFailed(req.JobID, req.MessageID, req.ArtIdx)
	// If this MessageID was already counted as a success, don't increment
	// partsWritten again (the failure ack above is still recorded).
	return !alreadyCounted
}

func (a *Assembler) handleSuccessArticle(f *openFile, req WriteRequest, wc *writeCache, open map[fileKey]*openFile, key fileKey) bool {
	if req.MessageID != "" {
		if f.seenDone == nil {
			f.seenDone = make(map[string]struct{})
		}
		if _, dup := f.seenDone[req.MessageID]; dup {
			a.recordPendingDone(req.JobID, req.MessageID, req.ArtIdx)
			if req.Data != nil {
				decoder.PutBuffer(req.Data)
			}
			return false
		}
		if f.seenFailed != nil {
			if _, was := f.seenFailed[req.MessageID]; was {
				a.writeArticleOrBuffer(f, key, req, wc, open)
				f.seenDone[req.MessageID] = struct{}{}
				a.recordPendingDone(req.JobID, req.MessageID, req.ArtIdx)
				return false
			}
		}
	}
	if !a.writeArticleOrBuffer(f, key, req, wc, open) {
		if req.MessageID != "" {
			if f.seenFailed == nil {
				f.seenFailed = make(map[string]struct{})
			}
			f.seenFailed[req.MessageID] = struct{}{}
			a.recordPendingFailed(req.JobID, req.MessageID, req.ArtIdx)
		}
	} else {
		if req.MessageID != "" {
			f.seenDone[req.MessageID] = struct{}{}
		}
		a.recordPendingDone(req.JobID, req.MessageID, req.ArtIdx)
		a.recordArticleCRC(f, req)
	}
	return true
}

// recordArticleCRC accumulates CRC information for a successfully written article.
func (a *Assembler) recordArticleCRC(f *openFile, req WriteRequest) {
	if req.CRC != 0 {
		f.crcParts = append(f.crcParts, crcPart{
			offset: req.Offset,
			crc:    req.CRC,
			len:    int64(len(req.Data)),
		})
	} else if len(req.Data) > 0 {
		f.crcValid = false
	}
}

func (a *Assembler) finalizeFile(f *openFile, key fileKey, req WriteRequest, open map[fileKey]*openFile, completed map[fileKey]struct{}, wc *writeCache) {
	// Drain any remaining cached articles for this file before close.
	a.drainCacheForFile(wc, f, key)
	// Truncate to the actual decoded size. Pre-allocation
	// (fallocate/ftruncate) extends the file to the NZB-declared
	// encoded size, which includes yEnc overhead (~2% larger).
	// Without this truncation the file has trailing zero bytes,
	// which causes par2 to report it as damaged.
	if f.maxWritten > 0 {
		if err := f.handle.Truncate(f.maxWritten); err != nil {
			a.log.Error("truncate completed file to decoded size",
				"path", f.info.Path,
				"expected", f.maxWritten,
				"error", err,
			)
		}
	} else {
		a.log.Debug("zero-length file completed",
			"path", f.info.Path,
		)
	}
	// Durability: fsync before closing and reporting completion.
	if err := f.handle.Sync(); err != nil {
		a.log.Error("fsync completed file",
			"path", f.info.Path,
			"error", err,
		)
	}
	if err := f.handle.Close(); err != nil {
		a.log.Warn("close completed file",
			"path", f.info.Path,
			"error", err,
		)
	}
	delete(open, key)
	delete(a.pendingCursor, key)
	completed[key] = struct{}{} // tombstone: reject late duplicates
	telemetry.FilesCompleted.Add(1)
	a.log.Info("file complete", "job", req.JobID, "fileidx", req.FileIdx, "path", f.info.Path)
	// Flush pending Done/Failed before firing the callback. The
	// pipeline's watchCompletions must not observe IsComplete()==true
	// on a file whose articles are not yet marked Done in the queue,
	// or it will race job-completion logic ahead of durability state.
	a.flush()

	// Compute the whole-file CRC32 by combining per-article CRCs
	// in offset order. This produces the same CRC as if the file
	// were read sequentially, which is what par2 files store.
	var fileCRC uint32
	if f.crcValid && len(f.crcParts) > 0 {
		slices.SortFunc(f.crcParts, func(a, b crcPart) int {
			return cmp.Compare(a.offset, b.offset)
		})
		fileCRC = f.crcParts[0].crc
		for _, p := range f.crcParts[1:] {
			fileCRC = crc32util.Combine(fileCRC, p.crc, p.len)
		}
		a.log.Debug("computed file CRC32",
			"job", req.JobID, "fileidx", req.FileIdx,
			"path", f.info.Path, "crc32", fileCRC,
			"parts", len(f.crcParts))
	}

	if a.opts.OnFileComplete != nil {
		a.opts.OnFileComplete(req.JobID, req.FileIdx, fileCRC)
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
		dir := filepath.Dir(f.info.Path)
		if _, already := seen[dir]; already {
			continue
		}
		seen[dir] = struct{}{}

		// This is a periodic background check inside the worker loop, not
		// tied to any request lifecycle, so there is no natural context to
		// thread through here — but it must still be bounded: this worker
		// owns all open file handles and is the only drainer of a.reqs, so
		// an uninterruptible statfs on a stuck mount would stall the whole
		// pipeline. See diskCheckTimeout.
		ctx, cancel := context.WithTimeout(context.Background(), diskCheckTimeout)
		free, err := a.diskProbe.FreeBytes(ctx, dir)
		cancel()
		if err != nil {
			a.log.Warn("disk-space check failed", "dir", dir, "error", err)
			continue
		}
		if free < a.minFreeBytes.Load() {
			a.opts.OnLowDisk(dir, free)
		}
	}
}

// writeArticleOrBuffer either buffers the article in the write cache (if
// enabled and coalescing is active) or writes it directly to disk. Returns
// true if the data was successfully written or buffered, false on I/O error.
//
// When caching is enabled, the article is buffered in memory. After each
// buffer insertion, contiguous runs are checked and flushed as a single
// coalesced WriteAt. Under memory pressure (>90% of limit), the file with
// the most buffered data is force-flushed.
func (a *Assembler) writeArticleOrBuffer(f *openFile, key fileKey, req WriteRequest, wc *writeCache, open map[fileKey]*openFile) bool {
	// Track the high-water mark of decoded bytes so we can truncate
	// the file to its true size at completion (see processRequest).
	if end := req.Offset + int64(len(req.Data)); end > f.maxWritten {
		f.maxWritten = end
	}

	if wc.buffer(key, req.Offset, req.Data) {
		telemetry.CacheHits.Add(1)
		// Article buffered. Check for a flushable contiguous run.
		if run := wc.flushContiguous(key); run != nil {
			if !a.flushRun(f, run) {
				return false
			}
			a.pendingCursor[key] = wc.cursorFor(key)
		}
		a.relievePressure(wc, open)
		return true
	}
	// Caching disabled — write directly.
	telemetry.DiskWrites.Add(1)
	telemetry.DiskWriteBytes.Add(int64(len(req.Data)))
	if _, err := f.handle.WriteAt(req.Data, req.Offset); err != nil {
		a.log.Error("write article",
			"path", f.info.Path,
			"offset", req.Offset,
			"error", err,
		)
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
		return false
	}
	if req.Data != nil {
		decoder.PutBuffer(req.Data)
	}
	return true
}

// flushRun writes a coalesced run to the target file and records telemetry.
// Returns true on success, false on I/O error.
func (a *Assembler) flushRun(f *openFile, run *flushRun) bool {
	telemetry.CacheFlushes.Add(1)
	telemetry.CacheFlushBytes.Add(int64(len(run.data)))
	telemetry.DiskWrites.Add(1)
	telemetry.DiskWriteBytes.Add(int64(len(run.data)))
	if _, err := f.handle.WriteAt(run.data, run.offset); err != nil {
		a.log.Error("write coalesced run",
			"path", f.info.Path,
			"offset", run.offset,
			"bytes", len(run.data),
			"error", err,
		)
		return false
	}
	return true
}

// relievePressure force-flushes the largest cached files until memory usage drops below the pressure threshold.
func (a *Assembler) relievePressure(wc *writeCache, open map[fileKey]*openFile) {
	for wc.pressure() {
		telemetry.CachePressureFlushes.Add(1)
		a.flushPressure(wc, open)
	}
}

func (a *Assembler) writeCachedArticles(f *openFile, arts []bufferedArticle, reason string) {
	for _, art := range arts {
		// Each drained article is its own WriteAt syscall (drains are not
		// coalesced), so count one DiskWrite per article — matching the
		// inline write paths in writeArticleOrBuffer.
		telemetry.DiskWrites.Add(1)
		telemetry.DiskWriteBytes.Add(int64(len(art.data)))
		if _, err := f.handle.WriteAt(art.data, art.offset); err != nil {
			a.log.Error(reason,
				"path", f.info.Path,
				"offset", art.offset,
				"error", err,
			)
		}
		if art.data != nil {
			decoder.PutBuffer(art.data)
		}
	}
}

// flushPressure force-flushes the file with the most buffered data to
// relieve memory pressure. Called from writeArticleOrBuffer when the
// cache exceeds 90% of its limit.
func (a *Assembler) flushPressure(wc *writeCache, open map[fileKey]*openFile) {
	fk, arts := wc.forceFlushLargest()
	if len(arts) == 0 {
		return
	}
	f, ok := open[fk]
	if !ok {
		a.log.Warn("pressure flush for unknown file",
			"jobID", fk.jobID, "fileIdx", fk.fileIdx,
			"articles", len(arts),
		)
		return
	}
	a.log.Debug("write cache pressure flush",
		"path", f.info.Path,
		"articles", len(arts),
		"used", wc.used,
		"limit", wc.limit,
	)
	a.writeCachedArticles(f, arts, "pressure flush write")
}

// drainCacheForFile writes all remaining cached articles for a file
// directly to disk. Called just before file completion/close.
func (a *Assembler) drainCacheForFile(wc *writeCache, f *openFile, key fileKey) {
	if !wc.enabled() {
		return
	}
	_, arts := wc.drainFile(key)
	a.writeCachedArticles(f, arts, "drain cached article to disk")
}

// flushWriteCache drains all cached articles across all files to disk.
// Called on assembler shutdown.
func (a *Assembler) flushWriteCache(wc *writeCache, open map[fileKey]*openFile) {
	if !wc.enabled() {
		return
	}
	allArts := wc.drainAll()
	for key, arts := range allArts {
		f, ok := open[key]
		if !ok {
			a.log.Warn("cached articles for unknown file on shutdown",
				"jobID", key.jobID, "fileIdx", key.fileIdx,
				"articles", len(arts),
			)
			continue
		}
		a.writeCachedArticles(f, arts, "flush cached article on shutdown")
	}
}
