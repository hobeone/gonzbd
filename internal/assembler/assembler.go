package assembler

import (
	"context"
	"math"

	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/storagefault"

	"github.com/hobeone/gonzbd/internal/telemetry"
)

// defaultQueueSize is the capacity of the internal write-request channel.
// Increased from the Python source's 12 to 2048 to absorb disk I/O spikes
// on high-speed (1Gbps+) connections.
const defaultQueueSize = 2048

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

	// syncOp, when non-nil, carries a barrier operation for the worker to
	// perform on its own goroutine. Set only by jobSyncTarget; never used by
	// ordinary write requests. See synctarget.go.
	syncOp *syncOp
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
	//
	// It no longer reports a whole-file CRC. The assembler cannot compute one
	// honestly: a resumed run is not sent the articles an earlier run
	// completed, so the parts it sees never tile the file (#349). The verified
	// figure is durability.FileExtent.PrefixCRC, guarded by HasPrefixCRC,
	// which the resumer derives from Class A over the file's real extent.
	//
	// The callback should be cheap; expensive work should be dispatched
	// asynchronously.
	OnFileComplete func(jobID string, fileIdx int)

	// OnLowDisk, if non-nil, is called when free space on the target
	// filesystem falls below MinFreeBytes. It is called on the worker goroutine
	// and should not block for long.
	OnLowDisk func(dir string, free int64)

	// MinFreeBytes is the low-disk threshold. Zero disables disk-space checks.
	MinFreeBytes int64

	// WriteCacheBytes is the memory limit for the write coalescing cache.
	// When positive, decoded articles are buffered in memory and flushed
	// as larger contiguous writes, reducing syscall count and improving
	// sequential write patterns. Zero disables caching (each article is
	// written individually, which is the pre-5.0 behavior).
	WriteCacheBytes int64
}

// fileKey uniquely identifies a target file within the assembler.
type fileKey struct {
	jobID   string
	fileIdx int
}

// openFile tracks an in-progress file being assembled.
//
// It is now bookkeeping only: the file's handle, its cache and every byte that
// moves belong to its FileWriter. What is left here is what the WORKER needs
// to route a request — where the file's writer is, how many of its parts have
// arrived, and what it was told about the file.
//
// maxWritten, crcParts and crcValid are gone. Each was a fact the assembler
// maintained about bytes on disk in order to truncate or to report a CRC, and
// both of those decisions moved to durability.Barrier, which is the only
// component that knows whether an fsync has happened. Keeping a shadow copy
// here is exactly the second-writer shape S5 forbids.
type openFile struct {
	w    *FileWriter
	info FileInfo
	// key identifies this file, so the write paths can record its resume
	// figures without threading the key through every call.
	key          fileKey
	partsWritten int
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
	a := &Assembler{
		log:       log.With("component", "assembler"),
		opts:      opts,
		reqs:      make(chan WriteRequest, opts.QueueSize),
		stopCh:    make(chan struct{}),
		diskProbe: NewDiskProbe(DefaultDiskProbeTTL),
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
// It performs no queue mutation of any kind. Articles it writes are reported
// to durability.Barrier when the barrier drains this assembler, and the
// barrier is the only component that acks them (X2). The batching that used to
// live here — pending Done/Failed maps flushed on a ticker — is gone with the
// acks: there is nothing left to batch.
func (a *Assembler) worker() {
	defer a.wg.Done()

	open := make(map[fileKey]*openFile)
	completed := make(map[fileKey]struct{})    // tombstone set for finished files
	cancelledJobs := make(map[string]struct{}) // tombstone set for cancelled jobs
	reqCount := 0
	wc := newWriteCache(a.opts.WriteCacheBytes)

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
	// step. Every open file is drained to disk and fsynced before its handle
	// closes, so a clean shutdown leaves nothing buffered.
	//
	// There is no ack here any more, and no ordering to get right between the
	// two: the articles this drain writes are reported by the next barrier,
	// and if the process dies before one runs they stay Outstanding and are
	// re-fetched. Losing that race used to mean marking articles complete
	// whose bytes were still only in the write cache; now it costs a
	// re-download, which is the direction the design trades toward.
	a.drainAndCloseAll(open)
	a.cacheUsedBytes.Store(wc.used)
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

	if req.JobID == "" && req.FileIdx == fileIdxSyncOp {
		// Control message: a barrier operation. Answered on this goroutine,
		// which owns every file handle and every write cache (X1).
		a.handleSyncOp(req.syncOp, open)
		return 0
	}
	if req.JobID == "" && req.FileIdx == -1 {
		// Control message: cancel a job. Close and remove all
		// open files for the job encoded in MessageID.
		cancelID := req.MessageID
		cancelledJobs[cancelID] = struct{}{}
		for k, f := range open {
			if k.jobID != cancelID {
				continue
			}
			_ = f.w.Close() //nolint:errcheck // best-effort; file is immediately removed
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
			a.drainAndClose(f)
			delete(open, k)
			completed[k] = struct{}{}
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

// drainAndClose flushes a file's buffered bytes, fsyncs, and closes it.
//
// The articles the drain writes are NOT acked here — this package has no ack
// authority. They are reported by the next Drain the barrier makes, or, if the
// process ends first, left Outstanding to be re-fetched (S3).
func (a *Assembler) drainAndClose(f *openFile) {
	if _, err := f.w.Drain(context.Background()); err != nil {
		a.log.Warn("drain file before close", "path", f.info.Path, "error", err)
	}
	if err := f.w.Sync(context.Background()); err != nil {
		a.log.Warn("sync file before close", "path", f.info.Path, "error", err)
	}
	if err := f.w.Close(); err != nil {
		a.log.Warn("close file", "path", f.info.Path, "error", err)
	}
}

// drainAndCloseAll drains and closes every remaining open file. Called on
// worker exit. Completion callbacks do NOT fire for partial files — writing
// N-of-M parts is not a completion event.
func (a *Assembler) drainAndCloseAll(open map[fileKey]*openFile) {
	for _, f := range open {
		a.drainAndClose(f)
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
		var err error
		f, err = a.openTargetFile(key, req, open, wc)
		if err != nil {
			// The article stays Outstanding: it is absent from every Drain, so
			// nothing claims it reached disk and a later run fetches it again.
			// The fault reaches the job through the barrier's Stallable rather
			// than being recorded against the article (A1).
			a.log.Error("open target file for article",
				"job", req.JobID, "fileidx", req.FileIdx, "error", err)
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
	} else if !a.handleSuccessArticle(f, req) {
		return
	}
	// Memory pressure is a whole-cache property, not a per-file one, so it is
	// relieved here rather than inside FileWriter: only the worker can see
	// every file and pick the largest. B2 bounds cached article memory by
	// configuration independently of job size, file size and job count, and
	// nothing else enforces that bound.
	a.relievePressure(wc, open)

	f.partsWritten++
	a.log.Debug("processed part",
		"job", req.JobID, "fileidx", req.FileIdx,
		"part", f.partsWritten, "total", f.info.TotalParts,
		"offset", req.Offset, "bytes", len(req.Data), "failed", req.FatalErr != nil)
	if f.info.TotalParts > 0 && f.partsWritten >= f.info.TotalParts {
		a.finalizeFile(f, key, req, open, completed, wc)
	}
}

// handleLateDuplicate handles articles arriving for a file that is already marked completed.
func (a *Assembler) handleLateDuplicate(req WriteRequest) {
	a.log.Debug("ignoring late article for completed file",
		"job", req.JobID, "fileidx", req.FileIdx, "msgid", req.MessageID)
	// No ack in either direction. A file this assembler has finished with is
	// one whose articles the barrier has already reported on, or will report
	// on from its own record; re-asserting anything about them here would be
	// this package claiming authority it no longer has.
	if req.Data != nil {
		decoder.PutBuffer(req.Data)
	}
}

// openTargetFile resolves file information and creates the target file on disk
// for a new fileKey.
//
// Every failure returns a classified *storagefault.Fault rather than logging
// and discarding the article (#357). The distinction matters because of A2: a
// dropped article is resolved neither way, so no later run re-dispatches it and
// the job stalls at 99% forever. A returned fault leaves it Outstanding, which
// costs a re-fetch and nothing else.
//
// A failing FileInfo resolver is classified against the path it could not
// produce, which is empty — the honest answer, since the resolver failed before
// there was a path. It is retryable by default under R18's "anything
// unrecognised is retryable" rule, which is the correct direction: a resolver
// failure is usually a job whose manifest is momentarily non-resident.
func (a *Assembler) openTargetFile(key fileKey, req WriteRequest, open map[fileKey]*openFile, wc *writeCache) (*openFile, error) {
	info, err := a.opts.FileInfo(req.JobID, req.FileIdx)
	if err != nil {
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
		return nil, storagefault.Classify("resolve", "", err)
	}

	dir := filepath.Dir(info.Path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
		return nil, storagefault.Classify("mkdir", info.Path, err)
	}
	//nolint:gosec // G304: path is caller-supplied from FileInfo resolver, which is responsible for safe derivation
	fh, err := os.OpenFile(info.Path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
		return nil, storagefault.Classify("open", info.Path, err)
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
	// No seeded high-water mark, and no seeded write cursor. Both used to
	// carry an earlier run's extent into this one so the completion truncate
	// would not cut away what those runs wrote (#342). The truncate no longer
	// derives its bound from anything this process measured — it comes from
	// the durable facts, which describe the FILE rather than the session — so
	// there is nothing left for a seed to protect.
	//
	// The cursor starts at 0 for the same reason it is safe to: it is a
	// coalescing hint, not an authority (#311, #353). A resumed file whose
	// early articles are not re-delivered simply never forms a contiguous run
	// from 0, and its articles are written individually instead. That costs
	// syscalls, never correctness.
	f := &openFile{
		w:    newFileWriter(fh, info.Path, key, wc),
		info: info,
		key:  key,
	}
	open[key] = f
	return f, nil
}

// handleFatalArticle counts a permanently failed article toward the file's
// part total without writing anything.
//
// It records no ack. A permanent failure is the queue's to record via
// AckPermanentFailure (R10), and this package no longer has an ack path in
// either direction. What is left here is the dedup that keeps partsWritten
// from overshooting TotalParts, which is purely local bookkeeping.
//
// Returns false when the article must not be counted again.
func (a *Assembler) handleFatalArticle(f *openFile, req WriteRequest) bool {
	a.log.Debug("counting failed article toward completion (skipping disk write)",
		"job", req.JobID, "fileidx", req.FileIdx, "path", f.info.Path, "error", req.FatalErr)
	w := f.w
	if _, dup := w.seenFailed[req.MessageID]; dup {
		return false
	}
	_, alreadyCounted := w.seenDone[req.MessageID]
	w.seenFailed[req.MessageID] = struct{}{}
	// An article already counted as a success must not increment the part
	// total a second time. The ack asymmetry that used to make this branch
	// delicate is gone with the acks themselves.
	return !alreadyCounted
}

// handleSuccessArticle hands one article's bytes to the file's writer.
//
// Returns false when the article must not be counted toward the file's part
// total — either it is a duplicate, or it is a retry of an article already
// counted as failed.
func (a *Assembler) handleSuccessArticle(f *openFile, req WriteRequest) bool {
	w := f.w
	id := articleID{msgID: req.MessageID, artIdx: req.ArtIdx}
	if req.MessageID != "" {
		if _, dup := w.seenDone[req.MessageID]; dup {
			// A duplicate of an article already accepted. Its first copy is
			// either still buffered or already written; either way this copy's
			// bytes are redundant, and re-writing them would be a second
			// WriteAt for the same range. Nothing is claimed here — the
			// barrier absorbs duplicate reports itself (R12).
			if req.Data != nil {
				decoder.PutBuffer(req.Data)
			}
			return false
		}
		if _, was := w.seenFailed[req.MessageID]; was {
			// A retry of an article already counted as failed. The false
			// return keeps partsWritten from counting it twice; the bytes are
			// still written, because they are still the file's content.
			w.seenDone[req.MessageID] = req.Offset
			a.acceptArticle(f, id, req)
			return false
		}
		// Recorded before the write is attempted, not after, so a write path
		// that fails can move the article to seenFailed without this function
		// putting it straight back.
		w.seenDone[req.MessageID] = req.Offset
	}
	a.acceptArticle(f, id, req)
	return true
}

// acceptArticle range-checks the write and hands it to the file's writer.
//
// A storage fault is logged and left to the barrier, which is what surfaces it
// to the job via Stallable. The article simply does not appear in the next
// Drain, which leaves it Outstanding (A1): a full disk is never recorded as
// article damage.
func (a *Assembler) acceptArticle(f *openFile, id articleID, req WriteRequest) {
	if !a.offsetInRange(f, req) {
		if req.Data != nil {
			decoder.PutBuffer(req.Data)
		}
		f.w.fail(id)
		return
	}
	if err := f.w.Accept(id, req.Offset, req.Data); err != nil {
		a.log.Error("write article", "path", f.info.Path, "offset", req.Offset, "error", err)
	}
}

// finalizeFile closes a file whose parts have all arrived.
//
// It no longer truncates and no longer computes a CRC. Both decisions moved to
// durability.Barrier: the truncate bound is the highest end offset among the
// file's DURABLE articles, which only the fact log knows, and the whole-file
// CRC is FileExtent.PrefixCRC guarded by HasPrefixCRC. Doing either here would
// be this package asserting something about bytes on disk that it cannot check
// — the shape behind #342, #349 and #350.
//
// What remains is: get every buffered byte written, fsync, close, and tell the
// caller the file is done so the barrier can finalize it.
func (a *Assembler) finalizeFile(f *openFile, key fileKey, req WriteRequest, open map[fileKey]*openFile, completed map[fileKey]struct{}, wc *writeCache) {
	a.drainAndClose(f)
	wc.forget(key)
	delete(open, key)
	completed[key] = struct{}{} // tombstone: reject late duplicates
	telemetry.FilesCompleted.Add(1)
	a.log.Info("file complete", "job", req.JobID, "fileidx", req.FileIdx, "path", f.info.Path)
	if a.opts.OnFileComplete != nil {
		a.opts.OnFileComplete(req.JobID, req.FileIdx)
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
// produce a file of arbitrary apparent size. The completion truncate no longer
// commits that size — it is bounded by the durable facts — but the sparse file
// itself is still the attack, so the offset is rejected before the write.
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

// relievePressure force-flushes the largest cached files until memory usage
// drops back under the threshold (B2).
//
// It writes through the owning file's FileWriter, so the flushed articles are
// reported Written exactly like any other write and reach the barrier through
// the same Drain. A file whose cache entry has no open writer cannot occur —
// FileWriter and the cache entry are created and dropped together — but the
// branch is kept and logged rather than assumed away, because the cost of
// being wrong is a silent leak of buffered bytes.
func (a *Assembler) relievePressure(wc *writeCache, open map[fileKey]*openFile) {
	for wc.pressure() {
		telemetry.CachePressureFlushes.Add(1)
		key, arts := wc.forceFlushLargest()
		if len(arts) == 0 {
			return
		}
		f, ok := open[key]
		if !ok {
			a.log.Warn("pressure flush for unknown file",
				"jobID", key.jobID, "fileIdx", key.fileIdx, "articles", len(arts))
			for _, art := range arts {
				if art.data != nil {
					decoder.PutBuffer(art.data)
				}
			}
			continue
		}
		for _, art := range arts {
			if err := f.w.writeOne(art); err != nil {
				a.log.Error("pressure flush write",
					"path", f.info.Path, "offset", art.offset, "error", err)
			}
		}
	}
}
