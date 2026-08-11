package assembler

import (
	"cmp"
	"context"
	"math"

	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/fsutil"

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

	// ExpectedSize is the NZB's declared *encoded* byte count for this file
	// (the sum of its segment `bytes` attributes). yEnc encoding inflates the
	// payload by ~2%, so this runs above the file's decoded size. When
	// positive, the assembler pre-allocates the file at this size on first
	// open, reducing per-write filesystem metadata overhead and fragmentation.
	// Zero disables pre-allocation.
	//
	// Pre-allocating to the encoded figure is why a completed file has to be
	// truncated back down to its decoded extent: the difference would
	// otherwise remain as trailing zeros, which par2 reports as damage. Both
	// consumers depend on this being the larger of the two figures — see
	// finalizeFile and offsetInRange.
	ExpectedSize int64

	// InitialWriteCursor seeds the write-coalescing cache's per-file cursor.
	// Zero for a fresh download (file starts at byte 0). On resume the caller
	// sets it to the file's persisted contiguous write frontier so coalescing
	// doesn't stall waiting for an offset-0 article that was already written
	// before the restart. See queue.FileProgress.WriteCursor.
	//
	// It doubles as a floor on the completed file's extent, for a file whose
	// earlier run predates InitialMaxWritten being persisted. It is a weaker
	// figure than that mark and is not a guarantee: drainFile advances the
	// cursor past everything it returns — gaps included, and before any write
	// is attempted — so a subsequent WriteAt that writeCachedArticles logs as
	// failed leaves the cursor above the bytes actually on disk. That is why
	// finalizeFile refuses to truncate upward rather than trusting either seed.
	InitialWriteCursor int64

	// InitialMaxWritten seeds the file's decoded high-water mark on resume.
	// Zero for a fresh download, and zero for a file whose earlier run predates
	// this figure being persisted — in which case InitialWriteCursor is the
	// only floor available, which under-counts a tail that arrived out of
	// order but never over-counts. See queue.FileProgress.MaxWritten.
	InitialMaxWritten int64
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
	// per-article CRCs in offset order. It is zero when no such CRC is
	// available: an article lacked CRC information (UU-encoded or failed), a
	// write failed, the file could not be trimmed to its decoded extent, or
	// this run's articles do not cover the whole file — the resume case,
	// where an earlier run's articles are not re-dispatched (#349). Consumers
	// must read zero as "unavailable", never as a mismatch.
	// The callback should be cheap; expensive work should be dispatched
	// asynchronously.
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

	// SetFileExtents persists a file's resume figures: the advanced contiguous
	// write frontier, and the highest byte position written. Called from the
	// worker's batched flush, never per-article — the two travel together so
	// the flush takes the queue's write lock once per file rather than twice.
	// Optional; nil disables persistence of both.
	//
	// An implementation must merge rather than overwrite, keeping the larger
	// of the stored and reported value for each figure. A flush reports
	// whichever figures the write paths that fired happen to know: a coalesced
	// run knows the new cursor, while a drained or uncached write knows only
	// the high-water mark and leaves the staged cursor unchanged — usually
	// zero, but whatever an earlier run in the same cycle staged. Storing a
	// report literally would erase the resume hint (#311) this call keeps.
	SetFileExtents func(jobID string, fileIdx int, cursor, maxWritten int64) error
}

// fileKey uniquely identifies a target file within the assembler.
type fileKey struct {
	jobID   string
	fileIdx int
}

// openFile tracks an in-progress file being assembled.
type openFile struct {
	handle *os.File
	info   FileInfo
	// key identifies this file, so the write paths can record its resume
	// figures without threading the key through every call.
	key          fileKey
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
	// seenDone dedupes successful writes symmetrically with seenFailed. The
	// value is the offset the article was accepted at, which the duplicate
	// branch needs in order to ask the write cache whether that copy's bytes
	// are still buffered — a duplicate is deduped by Message-ID and may carry
	// a different offset, so its own is the wrong one to look up.
	seenDone map[string]int64
	// crcParts accumulates per-article CRC32 values with their offsets, for
	// the articles this run wrote. At file completion they are sorted by
	// offset and combined with crc32util.Combine, which reconstructs the
	// whole-file CRC32 only when they tile the file's extent exactly — see
	// combineWholeFileCRC, which enforces that rather than assuming it.
	crcParts []crcPart
	// crcValid tracks whether a whole-file CRC is reportable for the bytes
	// this run put on disk. It is cleared by a failed article, a failed
	// write (inline or drained), an article with CRC=0 (UU-encoded), and by
	// any finalizeFile branch that leaves the file untrimmed — a failed
	// Stat, a truncate target above the file on disk, or a failed Truncate —
	// since the trailing zeros that survive there are not bytes the recorded
	// parts describe.
	//
	// It is not a statement about the file's coverage. A resumed run is not
	// sent the articles a previous run completed, so nothing fails and
	// nothing clears this flag, yet crcParts still describes only part of
	// the file. That gap is why the combination is guarded separately (#349).
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

	// pendingExtent holds the latest resume figures per file, flushed to
	// Options.SetFileExtents on the same cadence as pendingDone. Worker-owned.
	pendingExtent map[fileKey]fileExtent
}

// fileExtent is a file's persisted resume state: how far the contiguous write
// frontier has advanced, and the highest byte position written.
//
// Only maxWritten is a statement about bytes on disk. The cursor is advanced
// by drainFile across everything it hands back, gaps included and before any
// write is attempted, so it can outrun the file — see FileInfo.InitialWriteCursor
// and the guard in finalizeFile.
type fileExtent struct {
	cursor     int64
	maxWritten int64
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
		pendingExtent:      make(map[fileKey]fileExtent),
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
// once or after Stop.
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

// CloseJobHandles sends a control message to the worker goroutine to close all
// open file handles for the given job without deleting the files from disk,
// and blocks until the worker has actually done so. This is called when a job
// enters post-processing or Par2 repair, ensuring no open handles remain that
// would trigger NFS silly-rename (.nfs*) leaks when post-processing unlinks files.
func (a *Assembler) CloseJobHandles(ctx context.Context, jobID string) error {
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

	// Control message convention: JobID="" and FileIdx=-2, with the
	// real job ID in MessageID.
	ack := make(chan struct{})
	control := WriteRequest{
		JobID:     "",
		FileIdx:   -2,
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

	reqsClosed := false

mainLoop:
	for {
		select {
		case req, ok := <-a.reqs:
			if !ok {
				// Channel was closed; this path is not taken in normal operation
				// (we never close reqs), but defend against it. Breaks to the
				// shared shutdown block below rather than returning here, so
				// this path cannot skip flushWriteCache and drop cached bytes
				// that flush() has already reported Done.
				reqsClosed = true
				break mainLoop
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
			// The !ok arm breaks to the shared shutdown block rather than
			// duplicating it: a closed reqs makes this receive permanently
			// ready, so `default` would never be selected and this loop would
			// spin at full CPU dispatching zero-value requests forever. A zero
			// WriteRequest is not the cancel sentinel (JobID=="" && FileIdx==-1),
			// so it would reach processRequest with an empty job ID.
			//
			// Note `break drain` must be labelled: a bare break would exit the
			// select and fall straight back into this loop, which is the spin
			// it is meant to prevent.
		drain:
			for {
				select {
				case req, ok := <-a.reqs:
					if !ok {
						reqsClosed = true
						break drain
					}
					reqCount += a.dispatchRequest(req, open, completed, cancelledJobs, wc)
					if a.minFreeBytes.Load() > 0 && reqCount%diskCheckInterval == 0 {
						a.checkDiskSpace(open)
					}
				default:
					break drain
				}
			}
			break mainLoop
		}
	}

	// A closed reqs is not a normal shutdown — nothing closes it — and exiting
	// quietly would stop all assembly with no trace. Logged once here rather
	// than in each arm, since both reach this point.
	if reqsClosed {
		a.log.Error("assembler: reqs channel was closed; worker exiting, no further articles will be assembled")
	}

	// Shared shutdown, reached from every exit above so no path can skip a
	// step. Order matters: flush cached article bytes to disk BEFORE flush()
	// reports articles Done to the queue, or a shutdown could mark articles
	// complete whose bytes were still only in the write cache.
	a.flushWriteCache(wc, open)
	a.cacheUsedBytes.Store(wc.used)
	a.flush()
	a.closeAll(open)
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
			if err := fsutil.Remove(f.info.Path); err != nil && !os.IsNotExist(err) {
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
	if req.JobID == "" && req.FileIdx == -2 {
		// Control message: close all open file handles for a job without deleting files.
		targetID := req.MessageID
		for k, f := range open {
			if k.jobID != targetID {
				continue
			}
			a.drainCacheForFile(wc, f, k)
			_ = f.handle.Sync()  //nolint:errcheck // best-effort sync before closing
			_ = f.handle.Close() //nolint:errcheck // best-effort close
			delete(open, k)
			completed[k] = struct{}{}
		}
		a.flush()
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
		len(a.pendingDone) == 0 && len(a.pendingFailed) == 0 && len(a.pendingExtent) == 0 {
		return
	}
	if a.opts.MarkArticlesDoneByIdx != nil {
		for jobID, artIdxs := range a.pendingDoneByIdx {
			if err := a.opts.MarkArticlesDoneByIdx(jobID, artIdxs); err != nil {
				a.log.Debug("batch mark articles done by idx skipped (job removed or not resident)",
					"job", jobID, "count", len(artIdxs), "error", err)
			}
		}
	} else if a.opts.MarkArticlesDone != nil {
		for jobID, msgIDs := range a.pendingDone {
			if err := a.opts.MarkArticlesDone(jobID, msgIDs); err != nil {
				a.log.Debug("batch mark articles done skipped (job removed or not resident)",
					"job", jobID, "count", len(msgIDs), "error", err)
			}
		}
	}

	if a.opts.MarkArticlesFailedByIdx != nil {
		for jobID, artIdxs := range a.pendingFailedByIdx {
			if _, err := a.opts.MarkArticlesFailedByIdx(jobID, artIdxs); err != nil {
				a.log.Debug("batch mark articles failed by idx skipped (job removed or not resident)",
					"job", jobID, "count", len(artIdxs), "error", err)
			}
		}
	} else if a.opts.MarkArticlesFailed != nil {
		for jobID, msgIDs := range a.pendingFailed {
			if _, err := a.opts.MarkArticlesFailed(jobID, msgIDs); err != nil {
				a.log.Debug("batch mark articles failed skipped (job removed or not resident)",
					"job", jobID, "count", len(msgIDs), "error", err)
			}
		}
	}

	if a.opts.SetFileExtents != nil {
		for k, ext := range a.pendingExtent {
			if err := a.opts.SetFileExtents(k.jobID, k.fileIdx, ext.cursor, ext.maxWritten); err != nil {
				a.log.Debug("set file extents skipped (job removed or not resident)",
					"job", k.jobID, "fileidx", k.fileIdx, "error", err)
			}
		}
	}
	clear(a.pendingDone)
	clear(a.pendingFailed)
	clear(a.pendingDoneByIdx)
	clear(a.pendingFailedByIdx)
	clear(a.pendingExtent)
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
	// Seed the high-water mark from what earlier runs are recorded as having
	// written. Without this a resumed file's mark starts at zero and the
	// completion truncate cuts away the tail those runs already wrote (#342).
	//
	// Neither input is a guarantee — both describe a file this process has not
	// measured, and the cursor in particular can sit above the bytes actually
	// on disk. finalizeFile refuses to truncate upward rather than trusting
	// them; do not delete that guard on the strength of this seeding.
	f := &openFile{
		handle:     fh,
		info:       info,
		key:        key,
		crcValid:   true,
		maxWritten: max(info.InitialMaxWritten, info.InitialWriteCursor),
	}
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
	// Cross-check: if this MessageID was already counted as a success, it must
	// neither increment partsWritten again nor record a failure ack here. See
	// the note below the seenFailed insert for why the ack is suppressed.
	alreadyCounted := false
	if f.seenDone != nil {
		if _, was := f.seenDone[req.MessageID]; was {
			alreadyCounted = true
		}
	}
	f.seenFailed[req.MessageID] = struct{}{}
	if !alreadyCounted {
		a.recordPendingFailed(req.JobID, req.MessageID, req.ArtIdx)
	}
	// If this MessageID was already counted as a success, don't increment
	// partsWritten again, and leave the ack to the success path.
	//
	// The ack half is load-bearing since the Done moved to the write. This
	// used to record the failure too and lose harmlessly: both acks landed in
	// one flush, which applies the Done batch first, and markDone/markFailed
	// are first-writer-wins. Now the Done waits for the bytes, so a failure
	// recorded here is published a batch earlier and wins — marking an article
	// failed, charging failedBytes, and calling for a par2 repair on a file
	// that is complete on disk.
	return !alreadyCounted
}

func (a *Assembler) handleSuccessArticle(f *openFile, req WriteRequest, wc *writeCache, open map[fileKey]*openFile, key fileKey) bool {
	if req.MessageID != "" {
		if f.seenDone == nil {
			f.seenDone = make(map[string]int64)
		}
		if acceptedAt, dup := f.seenDone[req.MessageID]; dup {
			// Re-emit the ack so the queue receives it even if a prior flush
			// dropped it — but only once the first copy's bytes have left the
			// cache. While they are still buffered, seenDone means the article
			// was accepted, not that the file holds it, and re-emitting here
			// would put back exactly the premature Done this path removed.
			// The write that drains it will ack it.
			if !wc.buffered(key, acceptedAt) {
				a.recordPendingDone(req.JobID, req.MessageID, req.ArtIdx)
			}
			if req.Data != nil {
				decoder.PutBuffer(req.Data)
			}
			return false
		}
		if f.seenFailed != nil {
			if _, was := f.seenFailed[req.MessageID]; was {
				// A retry of an article already counted as failed. The false
				// return is what keeps partsWritten from counting it twice,
				// so it stays; only the ack moves into writeAndSettle with
				// every other write.
				f.seenDone[req.MessageID] = req.Offset
				a.writeAndSettle(f, key, req, wc, open)
				return false
			}
		}
		// Recorded before the write is attempted, not after, so that a write
		// path which fails can move the article to seenFailed without this
		// function putting it straight back.
		f.seenDone[req.MessageID] = req.Offset
	}
	a.writeAndSettle(f, key, req, wc, open)
	return true
}

// writeAndSettle writes or buffers an article and settles what the outcome
// owes the queue.
//
// The three outcomes divide by who owns the ack, which is the whole point of
// the type. Only the uncached paths settle here; once an article is buffered,
// the cache owns both its ack and its seen-set membership until its bytes
// move, and this function deliberately does nothing about them.
//
// recordArticleCRC fires for a buffered article as well as a written one. Its
// input is req.CRC, which the identity carried through the cache does not
// include, so deferring it with the ack would leave every cached file without
// a whole-file CRC. A part recorded for bytes that then fail to land is
// covered instead by crcValid, which every failing write path that still has
// the file clears (#349). failDrainedArticles is the exception, and only
// because its file has already left the open map, so nothing remains to
// finalize a CRC for it.
func (a *Assembler) writeAndSettle(f *openFile, key fileKey, req WriteRequest, wc *writeCache, open map[fileKey]*openFile) {
	switch a.writeArticleOrBuffer(f, key, req, wc, open) {
	case outcomeDurable:
		a.recordPendingDone(req.JobID, req.MessageID, req.ArtIdx)
		a.recordArticleCRC(f, req)
	case outcomeDeferred:
		a.recordArticleCRC(f, req)
	default:
		// outcomeFailed, and anything a later edit adds without saying what it
		// means. Go does not check a switch over a named type for
		// exhaustiveness and this repository enables no linter that does, so
		// the unhandled case has to land somewhere by construction. It lands
		// on the failing branch: the cost of a needless re-fetch is a wasted
		// download, and the cost of a wrong Done is a file that is silently
		// short forever.
		//
		// The file is now missing this part's bytes, so a whole-file CRC
		// combined from the parts that did land would describe a byte range
		// the file does not have. Report it as unavailable instead:
		// quickcheck then classifies the file as NoCRC ("could not be
		// checked") rather than Mismatched ("corrupted"). Both route to par2
		// repair, but only one of them is true.
		//
		// This also covers the retry path in handleSuccessArticle: once any
		// part has failed the file CRC stays invalid, even if a later attempt
		// writes successfully.
		f.crcValid = false
		a.failArticle(f, articleID{msgID: req.MessageID, artIdx: req.ArtIdx})
	}
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

// combineWholeFileCRC folds per-article CRCs into the CRC of the whole file.
//
// crc32util.Combine reconstructs CRC(A||B) from the two parts' CRCs and B's
// length alone; it is given no offsets, so it cannot tell an adjacent part from
// a distant one. The fold below therefore yields the CRC of the parts
// concatenated, which is the CRC of the file only when they tile [0, total)
// exactly. Skipping bytes is not harmless even when those bytes are zero:
// appending n bytes multiplies the CRC state by M^n whatever their value.
//
// parts must already be sorted by offset. ok is false whenever they do not tile
// exactly — a hole below a part, an overlap, or a span that does not end
// precisely at total, whether short of it or past it. In
// practice that is nearly always the resume case, where articles a previous run
// completed are not re-dispatched and so contribute no part (#349); an overlap
// would instead mean a defect upstream in the dedup or CRC-recording path, so
// do not read a false here as proof the file was resumed.
//
// An empty parts slice tiles a zero-length file, so it reports ok — callers
// distinguishing "no CRC recorded" from "verified empty" must check that
// themselves, as finalizeFile does with its len(f.crcParts) > 0 guard.
func combineWholeFileCRC(parts []crcPart, total int64) (uint32, bool) {
	var (
		crc  uint32
		next int64
	)
	for _, p := range parts {
		// Seeding from 0 rather than parts[0].crc is what makes this check
		// cover the first part too: Combine(0, c, n) == c, so the old seed
		// silently assumed parts[0] began at offset 0.
		if p.offset != next {
			return 0, false // a hole below this part, or an overlap
		}
		crc = crc32util.Combine(crc, p.crc, p.len)
		next += p.len
	}
	if next != total {
		return 0, false // the parts stop short of the file's extent
	}
	return crc, true
}

func (a *Assembler) finalizeFile(f *openFile, key fileKey, req WriteRequest, open map[fileKey]*openFile, completed map[fileKey]struct{}, wc *writeCache) {
	// Drain any remaining cached articles for this file before close.
	a.drainCacheForFile(wc, f, key)
	// Truncate to the actual decoded size. Pre-allocation
	// (fallocate/ftruncate) extends the file to the NZB-declared
	// encoded size, which includes yEnc overhead (~2% larger).
	// Without this truncation the file has trailing zero bytes,
	// which causes par2 to report it as damaged.
	//
	// Never grow the file. maxWritten is seeded from state persisted by an
	// earlier run, describing a file this process has not measured, so it can
	// exceed what is actually on disk — the download directory may have been
	// removed between runs, pre-allocation may have failed and been logged
	// rather than fatal, or a persisted write cursor may have been advanced
	// by drainFile across a range whose WriteAt then failed. Truncating to it
	// would append exactly the trailing zeros this call exists to remove, and
	// a job with no par2 has no repair stage to notice. Trimming is the only
	// direction that was ever intended.
	//
	// Each outcome logs for itself rather than routing through the
	// zero-length case: a file skipped because the target overshoots is not
	// empty, and saying so would misdescribe the one event an operator has to
	// go on.
	//
	// The three failure branches also clear crcValid: the pre-allocated
	// trailing zeros survive there, so the bytes on disk are not the
	// [0, maxWritten) the recorded parts describe, and reporting a CRC over
	// that range would hand par2 a mismatch on a file nothing is actually
	// wrong with — the same false corruption claim #349 removes for resumed
	// files. The zero-length branch is not one of them: it is not a failure,
	// nothing was written, and there is no extent for a CRC to describe.
	size, statErr := f.handle.Stat()
	switch {
	case f.maxWritten <= 0:
		a.log.Debug("zero-length file completed", "path", f.info.Path)
	case statErr != nil:
		a.log.Warn("stat completed file; skipping truncate",
			"path", f.info.Path, "error", statErr)
		f.crcValid = false
	case f.maxWritten > size.Size():
		a.log.Debug("truncate target exceeds the file on disk; leaving it alone",
			"path", f.info.Path, "target", f.maxWritten, "size", size.Size())
		f.crcValid = false
	default:
		if err := f.handle.Truncate(f.maxWritten); err != nil {
			f.crcValid = false
			a.log.Error("truncate completed file to decoded size",
				"path", f.info.Path,
				"expected", f.maxWritten,
				"error", err,
			)
		}
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
	// were read sequentially, which is what par2 files store — but only
	// when this run recorded a part for every byte of the file. When it
	// did not, there is no whole-file CRC to report and fileCRC stays 0,
	// which quickcheck reads as NoCRC ("unavailable") rather than as a
	// mismatch.
	var fileCRC uint32
	if f.crcValid && len(f.crcParts) > 0 {
		slices.SortFunc(f.crcParts, func(a, b crcPart) int {
			return cmp.Compare(a.offset, b.offset)
		})
		if crc, ok := combineWholeFileCRC(f.crcParts, f.maxWritten); ok {
			fileCRC = crc
			a.log.Debug("computed file CRC32",
				"job", req.JobID, "fileidx", req.FileIdx,
				"path", f.info.Path, "crc32", fileCRC,
				"parts", len(f.crcParts))
		} else {
			a.log.Debug("partial article coverage; reporting file CRC as unavailable",
				"job", req.JobID, "fileidx", req.FileIdx,
				"path", f.info.Path, "parts", len(f.crcParts),
				"extent", f.maxWritten)
		}
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

// offsetSlackDivisor bounds how far past FileInfo.ExpectedSize a write may
// land before it is rejected. ExpectedSize is the NZB's declared *encoded*
// byte count, which already runs ~2% above the decoded size (see
// preallocateFile), so a legitimate decoded write never exceeds it. NZB
// `bytes` attributes are advisory though, so allow ExpectedSize/8 (12.5%)
// of slack rather than treating it as an exact bound.
const offsetSlackDivisor = 8

// offsetInRange reports whether a write request's target range is plausible
// for the file it claims to belong to.
//
// req.Offset originates from the yEnc `=ypart begin=` header, which is parsed
// as an unbounded int64 from the article body returned by the NNTP server. It
// is therefore attacker-controlled: without this check, a hostile or
// compromised server can return a single article whose offset makes WriteAt
// produce a file of arbitrary apparent size (finalizeFile then truncates to
// that inflated high-water mark, committing it).
func (a *Assembler) offsetInRange(f *openFile, req WriteRequest) bool {
	reject := func(reason string) bool {
		a.log.Warn("rejecting out-of-range article write offset",
			"path", f.info.Path,
			"offset", req.Offset,
			"bytes", len(req.Data),
			"expected_size", f.info.ExpectedSize,
			"reason", reason,
		)
		telemetry.PipelineErrors.Add(telemetry.ErrClassDiskWriteError, 1)
		return false
	}

	if req.Offset < 0 {
		return reject("negative offset")
	}

	end := req.Offset + int64(len(req.Data))
	if end < req.Offset {
		return reject("offset+length overflows int64")
	}

	// ExpectedSize == 0 means the NZB did not declare a size; the overflow
	// and negative checks above are all we can enforce.
	if f.info.ExpectedSize <= 0 {
		return true
	}
	// Guard the slack arithmetic itself against a degenerate ExpectedSize.
	if f.info.ExpectedSize > math.MaxInt64-(f.info.ExpectedSize/offsetSlackDivisor) {
		return true
	}
	if limit := f.info.ExpectedSize + f.info.ExpectedSize/offsetSlackDivisor; end > limit {
		return reject("write extends past declared file size")
	}
	return true
}

// writeOutcome reports what happened to an article, and with it who owes the
// queue an ack. Bytes that reached WriteAt and bytes that are merely buffered
// are different answers to that question, and conflating them is #355.
type writeOutcome int

const (
	// outcomeFailed means the bytes are not on disk and will not be. The
	// caller fail-acks the article and invalidates the file CRC.
	//
	// It is deliberately the zero value: an outcome left unset by some future
	// edit resolves to a re-fetch, never to a silent Done.
	outcomeFailed writeOutcome = iota
	// outcomeDurable means the bytes reached WriteAt. The caller acks Done.
	outcomeDurable
	// outcomeDeferred means the bytes are buffered in the write cache. The
	// caller must not ack the article in either direction; one of the write
	// paths will, when the bytes actually move.
	//
	// The caller still records the article as accepted in seenDone before
	// handing it over — that is what "accepted" means, and partsWritten
	// counts it. What passes to the cache is every transition after that
	// point, including moving the article to seenFailed if its write is lost.
	outcomeDeferred
)

// writeArticleOrBuffer either buffers the article in the write cache (if
// enabled and coalescing is active) or writes it directly to disk. The
// returned outcome says whether the bytes reached disk, are still buffered, or
// are gone — and with that, who owes the queue an ack.
//
// When caching is enabled, the article is buffered in memory. After each
// buffer insertion, contiguous runs are checked and flushed as a single
// coalesced WriteAt. Under memory pressure (>90% of limit), the file with
// the most buffered data is force-flushed.
func (a *Assembler) writeArticleOrBuffer(f *openFile, key fileKey, req WriteRequest, wc *writeCache, open map[fileKey]*openFile) writeOutcome {
	if !a.offsetInRange(f, req) {
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
		return outcomeFailed
	}

	art := bufferedArticle{
		offset: req.Offset,
		data:   req.Data,
		id:     articleID{msgID: req.MessageID, artIdx: req.ArtIdx},
	}
	if cached, displaced := wc.buffer(key, art); cached {
		telemetry.CacheHits.Add(1)
		// An article evicted from this offset is settled as failed so it is
		// fetched again; nothing else will write its bytes now. Its CRC part
		// was recorded when it was accepted and describes a length the
		// replacement does not have to match, so the file's CRC goes with it.
		for _, id := range displaced {
			f.crcValid = false
			a.failArticle(f, id)
		}
		// Article buffered. Check for a flushable contiguous run.
		if run := wc.flushContiguous(key); run != nil {
			// A failed run is not reported to the caller. flushRun has
			// already settled every article it coalesced, this one included
			// if it was contiguous with the cursor — and if it was not, its
			// bytes are still buffered and still owed a later ack. Returning
			// failed here would fail-ack an article whose bytes are fine,
			// which markFailed makes permanent: it and markDone both set the
			// done bit, and both no-op once it is set, so the first ack wins.
			if a.flushRun(f, run) {
				a.pendingExtent[key] = fileExtent{
					cursor:     wc.cursorFor(key),
					maxWritten: f.maxWritten,
				}
			}
		}
		a.relievePressure(wc, open)
		return outcomeDeferred
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
		telemetry.PipelineErrors.Add(telemetry.ErrClassDiskWriteError, 1)
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
		return outcomeFailed
	}
	a.noteWritten(f, req.Offset, int64(len(req.Data)))
	if req.Data != nil {
		decoder.PutBuffer(req.Data)
	}
	return outcomeDurable
}

// ackArticleDone settles an article the file now holds on disk.
func (a *Assembler) ackArticleDone(f *openFile, id articleID) {
	a.recordPendingDone(f.key.jobID, id.msgID, id.artIdx)
}

// failArticle settles an article whose bytes did not reach disk and will not,
// so a later run fetches it again.
//
// It moves the article out of seenDone as well as acking, because the two
// answer different questions that this file's dedup logic would otherwise
// conflate: seenDone means "accepted, counted toward TotalParts", and after a
// failed write the article is still counted but is no longer on its way to
// disk. Leaving it there would let the duplicate branch in handleSuccessArticle
// re-emit a Done for bytes the file does not have.
func (a *Assembler) failArticle(f *openFile, id articleID) {
	if id.msgID != "" {
		delete(f.seenDone, id.msgID)
		if f.seenFailed == nil {
			f.seenFailed = make(map[string]struct{})
		}
		f.seenFailed[id.msgID] = struct{}{}
	}
	a.recordPendingFailed(f.key.jobID, id.msgID, id.artIdx)
}

// failDrainedArticles settles articles drained out of the cache for a file
// that is no longer open, and returns their buffers to the pool.
//
// Neither caller is believed reachable: every delete from the open-file map is
// paired with a forget or a drain in the same call, with no yield to the
// request channel in between. Nothing enforces that, and both call sites log
// precisely because someone doubted it, so this settles what it discards
// rather than trusting the argument.
//
// Settling matters here because an article acked neither Done nor Failed is
// one no later run re-dispatches. Failing them keeps the worst case a
// re-fetch.
//
// There is no openFile to update, so unlike failArticle this cannot correct
// the seen sets — the file they belong to is gone.
func (a *Assembler) failDrainedArticles(key fileKey, arts []bufferedArticle) {
	for _, art := range arts {
		a.recordPendingFailed(key.jobID, art.id.msgID, art.id.artIdx)
		if art.data != nil {
			decoder.PutBuffer(art.data)
		}
	}
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
			"articles", len(run.ids),
			"error", err,
		)
		telemetry.PipelineErrors.Add(telemetry.ErrClassDiskWriteError, 1)
		// Every article in the run loses its bytes here, not just whichever
		// one happened to trigger the flush: buildContiguousRun coalesced
		// them all into one buffer and pooled the originals before this
		// write was attempted. Settling only the triggering article would
		// leave the rest marked Done from their own accept-time acks, with
		// their bytes freed and no run able to fetch them again (#355).
		f.crcValid = false
		for _, id := range run.ids {
			a.failArticle(f, id)
		}
		return false
	}
	a.noteWritten(f, run.offset, int64(len(run.data)))
	for _, id := range run.ids {
		a.ackArticleDone(f, id)
	}
	return true
}

// noteWritten raises the file's decoded high-water mark to cover a range that
// has just reached WriteAt, and stages that mark for the next batched flush.
// It leaves the staged cursor alone: this path knows nothing about the
// contiguous frontier, which only a coalesced run advances.
//
// It is called from the write paths rather than from the point an article is
// accepted, deliberately. The mark seeds a resumed file's truncate target, so
// it must describe bytes that are on disk; a mark persisted ahead of the bytes
// would make a later truncate extend a short file with zeros that nothing
// fills.
//
// That reasoning used to run through the article ack — an article acked Done
// while still buffered is never refetched, so the gap could never be closed.
// The ack no longer moves ahead of the bytes (#355), so the mark no longer
// depends on it. Staging only from the write paths remains correct on its own
// terms, and is the same rule the ack now follows.
func (a *Assembler) noteWritten(f *openFile, offset, n int64) {
	if end := offset + n; end > f.maxWritten {
		f.maxWritten = end
	}
	// New always allocates this map, so the nil case is unreachable in
	// production; it exists for the tests that build an Assembler as a literal
	// to exercise a single helper. Creating it rather than assuming it is one
	// branch on a path that already writes to the map, against a nil-map panic
	// on the single worker goroutine — which owns every open file handle and
	// is the only drainer of a.reqs, so it would stop the pipeline rather than
	// fail one write.
	if a.pendingExtent == nil {
		a.pendingExtent = make(map[fileKey]fileExtent)
	}
	ext := a.pendingExtent[f.key]
	ext.maxWritten = f.maxWritten
	a.pendingExtent[f.key] = ext
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
			telemetry.PipelineErrors.Add(telemetry.ErrClassDiskWriteError, 1)
			// The file is now missing this article's bytes, so a whole-file
			// CRC combined from the parts that did land would describe a byte
			// range the file does not have. The inline write path clears this
			// for the same condition; a drained write is no different. Without
			// it a later article can raise maxWritten past the failed range and
			// the parts still tile it, producing a confident CRC over content
			// the disk lacks (#349).
			f.crcValid = false
			a.failArticle(f, art.id)
		} else {
			a.noteWritten(f, art.offset, int64(len(art.data)))
			a.ackArticleDone(f, art.id)
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
		a.failDrainedArticles(fk, arts)
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
	// drainFile retains the entry so a pressure flush keeps its advanced
	// cursor (#311). Both callers here are closing the handle, so the cursor
	// has no further use in this pass and the entry would otherwise
	// accumulate, one per file, until shutdown.
	//
	// The key can legitimately come back: CloseJobHandles runs at
	// post-processing start, and a job retried from history is rebuilt under
	// the same ID and file indices, so it re-enters on an identical fileKey.
	// That is why this drops the entry rather than zeroing it — initCursor
	// reseeds from the persisted write cursor on re-entry, which is the
	// correct resume point, whereas a stale in-memory cursor would not be.
	wc.forget(key)
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
			a.failDrainedArticles(key, arts)
			continue
		}
		a.writeCachedArticles(f, arts, "flush cached article on shutdown")
	}
}
