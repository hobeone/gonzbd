package assembler

import (
	"context"
	"fmt"
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
	// JobID, FileIdx, ArtIdx and MessageID identify the article, and on the
	// article path the CALLER DOES NOT OWN THEM: WriteArticle overwrites all
	// four from its ArticleRef argument, and a value supplied here is
	// discarded. Setting them on a request destined for WriteArticle has no
	// effect. See ArticleRef for why the identity is a parameter.
	//
	// They stay on this struct because control messages are WriteRequest
	// values too, and those DO own them: a control message carries one of the
	// negative FileIdx sentinels (see synctarget.go) with JobID empty and the
	// job named in MessageID, which is how dispatchRequest tells an operation
	// from an article before any writer sees it.
	JobID string

	// FileIdx is the index into the job's Files slice on the article path, or
	// a control-message sentinel. See the block above.
	FileIdx int

	// ArtIdx is the article's index in the job's manifest. Unused by control
	// messages. See the block above.
	ArtIdx int32

	// MessageID is the article's NNTP Message-ID on the article path, and the
	// job ID on a control message. The assembler uses the former to mark the
	// article Done (on success, after fsync) or Failed (on FatalErr) in the
	// queue. See the block above.
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

	// ackCh, when non-nil, is closed by the worker immediately after this
	// control message (cancel) has been fully processed -- i.e. after the
	// job's open file handles have been closed and removed. Set only by
	// CancelJob; never used by ordinary write requests.
	// It carries an error for the control messages that can fail. A sender
	// with nothing to report closes it without sending, so a receiver reading
	// `<-ackCh` gets a nil error either way — which is what lets the cancel
	// arm, which cannot fail, stay a bare close.
	ackCh chan error

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
	// TotalParts for a file have been written.
	//
	// The handle is still OPEN at that point, and stays open until the caller
	// releases it with CloseFile. That is the handoff durability.Barrier's
	// FinalizeFile needs: it has to Drain, Sync, Truncate and Stat the file,
	// and all four go through this package's handle. See finalizeFile.
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

	// OnWriteFault, if non-nil, is called on the worker goroutine when a
	// write for an article fails.
	//
	// It exists because a fault raised inside FileWriter.Accept has no other
	// way out. The barrier only sees what Drain, Sync, Stat or Truncate
	// returns, and a rejected write leaves nothing behind for a later Drain to
	// fail on — with the cache disabled there is nothing buffered at all. The
	// article is not failed by this: a storage fault says nothing about the
	// article's availability (A1), so the caller stalls the job and returns
	// the article to Outstanding.
	//
	// It carries NO article index, and that separation is the fix for a whole
	// class of stranding. The signature used to name one article, so a batch
	// failure — a coalesced run, a drain, a cache displacement — could report
	// only whichever article triggered it, and the rest were rolled back
	// silently. Returning articles to Outstanding is now OnArticlesUnwritten's
	// job, which takes the whole set.
	OnWriteFault func(jobID string, fileIdx int, f *storagefault.Fault)

	// OnArticlesUnwritten, if non-nil, is called on the worker goroutine with
	// every article a failed write rolled back. The caller returns them to
	// Outstanding by clearing their Emitted bits.
	//
	// It is separate from OnWriteFault because the two are needed in different
	// combinations, not because the split is tidier. A fault this package
	// routes itself needs both; a Drain or Sync failure reaches the barrier,
	// which routes the fault — but the article set never leaves this package,
	// so it still needs this one. Folding them together meant either
	// double-routing the fault or losing the articles, and losing the
	// articles is what happened.
	//
	// Emitted is NOT Outstanding: ForEachUnfinishedArticle skips a set Emitted
	// bit, so an article reported by neither is stranded for the life of the
	// process — not Done, not Failed, not Outstanding — and only a restart's
	// ClearAllEmitted recovers it.
	//
	// No fault is passed, deliberately. This says nothing about why the write
	// failed and must not be read as evidence about any article (A1).
	OnArticlesUnwritten func(jobID string, fileIdx int, artIdxs []int32)

	// OnArticleRejected reports an article this package refused to write
	// because the article itself is not usable — currently only an
	// out-of-range yEnc offset (see offsetInRange).
	//
	// It is deliberately NOT OnWriteFault. A1 draws the line this pair sits
	// on: a storage fault says nothing about the article and resolves against
	// storage, while this says nothing about storage and resolves against the
	// article. Routing a rejection through OnWriteFault would stall the job on
	// a healthy disk; routing a disk fault through here would record a
	// perfectly good article as damaged.
	//
	// The article is permanently failed from this package's point of view —
	// the offset comes from the server and a re-fetch yields the same one —
	// so the caller records it with Queue.AckPermanentFailure. That is what
	// charges its bytes against the job's par2 recovery budget and releases
	// on-demand recovery volumes. Without it the article stays Emitted, which
	// is not Outstanding: ForEachUnfinishedArticle skips a set Emitted bit, so
	// the job waits forever on an article nothing will re-dispatch.
	OnArticleRejected func(jobID string, fileIdx int, artIdx int32, reason string)

	// OnPostAnomaly reports something structurally wrong with what the servers
	// served for a job, in terms a user can act on. Currently only an offset
	// collision: two segments of one file claiming the same byte range.
	//
	// It is per-JOB and pairs with OnArticleRejected rather than replacing it.
	// The rejection resolves each article's accounting; this explains the
	// SHAPE of the fault once, so the user learns their download is failing
	// because the post is malformed rather than because of a disk or network
	// problem — which is the whole of #379.
	//
	// The reason states the fact and stops there. It does not say the post is
	// bad, because this package cannot know that. Three things produce the
	// same observation: a genuinely malformed post, a redundant posting whose
	// copies are both valid, and a server mangling the =ypart begin= digits in
	// transit — and nothing here can separate them, because yEnc checksums the
	// decoded payload and never its headers. Option D's original wording
	// accused the post, on the strength of every eligible server agreeing;
	// with no failover there is no cross-server evidence to support it.
	//
	// Fired once per file (see faultedArticle.firstCollision), because the
	// caller's sink is a single overwritable job.Warning and an obfuscated
	// post can collide on every segment it has.
	OnPostAnomaly func(jobID string, fileIdx int, reason string)

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
// to route a request — where the file's writer is, and what it was told about
// the file.
//
// maxWritten, crcParts and crcValid are gone. Each was a fact the assembler
// maintained about bytes on disk in order to truncate or to report a CRC, and
// both of those decisions moved to durability.Barrier, which is the only
// component that knows whether an fsync has happened. Keeping a shadow copy
// here is exactly the second-writer shape S5 forbids.
//
// partsWritten went the same way, for the same reason at a smaller scale. It
// is derived from the writer's seenDone and seenFailed sets, and holding it
// here made every mutation a synchronisation obligation between two structs
// that nothing enforced. It now lives on FileWriter and is read through
// parts().
type openFile struct {
	w    *FileWriter
	info FileInfo
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
	// (drainAndCloseAll in worker()), so it stays accurate through the
	// only two places writeCache.used can change.
	cacheUsedBytes atomic.Int64

	// putBuffer, when non-nil, replaces decoder.PutBuffer as this assembler's
	// buffer-release path. Same discipline as diskProbe.statfs above:
	// same-package, set once before the instance is started.
	//
	// It exists because releasing a buffer is otherwise UNOBSERVABLE. The
	// destination is decoder's process-global sync.Pool, and sync.Pool is
	// emptied at every GC — so "put it, then get it back" is not a test of
	// whether Put was called, it is a race against the collector. A test built
	// that way reported a leak on 4 of 12 package runs under -race while the
	// code was correct. No retry count fixes it: retrying only widens the
	// window in which a GC can occur.
	putBuffer func([]byte)

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
		putBuffer: decoder.PutBuffer,
	}
	a.minFreeBytes.Store(opts.MinFreeBytes)
	return a
}

// SetMinFreeBytes updates the low-disk threshold without restarting the
// assembler. Zero disables disk-space checks. Thread-safe.
func (a *Assembler) SetMinFreeBytes(v int64) { a.minFreeBytes.Store(v) }

// MinFreeBytes returns the current low-disk threshold in bytes. Thread-safe.
func (a *Assembler) MinFreeBytes() int64 { return a.minFreeBytes.Load() } //nocover: trivial atomic load

// releaseBuffer returns a decoded article's buffer to the decoder pool.
//
// Every buffer release in this file goes through here so there is exactly one
// place a test can observe, and so a direct &Assembler{...} construction (which
// several tests use) still releases buffers rather than leaking them silently.
// The nil check is what makes that safe; it is not defensive padding.
func (a *Assembler) releaseBuffer(buf []byte) {
	if a.putBuffer != nil {
		a.putBuffer(buf)
		return
	}
	decoder.PutBuffer(buf)
}

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

// ArticleRef is the identity of the article a WriteArticle call carries: which
// job, which file, which article, and the Message-ID it was fetched by.
//
// It is a separate parameter rather than four fields the caller may leave unset
// on WriteRequest, because ArtIdx has no invalid value. A Message-ID is empty or
// it is not, so an omitted one is a loud error; an omitted ArtIdx is
// indistinguishable from a deliberate article 0. Taking the identity as a
// required parameter makes omission a compile error at every caller outside this
// package. See docs/article-validation-contract.md §4.
//
// The identity is un-omittable, not unrepresentable: a caller passing a zero
// ArtIdx deliberately cannot be told apart from one who defaulted it. Read the
// guarantee as "someone supplied an identity", not "the identity is valid".
type ArticleRef struct {
	// JobID identifies the parent download job.
	JobID string

	// FileIdx is the index into the job's Files slice.
	FileIdx int

	// ArtIdx is the global index of the article within the job's manifest.
	ArtIdx int32

	// MessageID is the article's NNTP Message-ID.
	MessageID string
}

// WriteArticle enqueues req for writing, under the identity in ref. It blocks
// until the worker accepts the request or ctx is cancelled. Returns ErrStopped
// if Stop has been called. Returns ctx.Err() if ctx is cancelled while waiting
// for channel capacity.
//
// ref is authoritative: the request's own identity fields are overwritten from
// it and are not read on this path. They remain on WriteRequest because the
// control-message convention builds WriteRequest values carrying FileIdx
// sentinels, and those do not pass through here.
//
// The identity is applied after the started/stopped checks, so a caller probing
// those errors with a bare request still gets them.
func (a *Assembler) WriteArticle(ctx context.Context, ref ArticleRef, req WriteRequest) error {
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

	// The ref owns the identity. Applying it here rather than trusting the
	// request's own fields is the whole point of the parameter: there is no
	// path from outside this package that reaches the worker with an identity
	// nobody supplied.
	req.JobID = ref.JobID
	req.FileIdx = ref.FileIdx
	req.ArtIdx = ref.ArtIdx
	req.MessageID = ref.MessageID

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
	// A context that is ALREADY cancelled resolves here, before the selects
	// below, and that is a correctness requirement rather than a fast path.
	//
	// Both selects have ctx.Done() ready from the start, and Go picks
	// uniformly at random among ready cases. So the first select had a ~50%
	// chance of enqueueing the control message anyway, and if the worker then
	// acked before the second select was evaluated, that select could pick
	// <-ack and return nil — reporting success for a call the caller had
	// already cancelled, after doing the work it asked not to be done.
	//
	// It is a real race, not a test artifact: the equivalent test on CloseJobHandles asserted
	// context.Canceled and failed about 1 run in 20 at package scope under
	// -race. Measured directly at -count=2000, the rate moved from 8/8000 to
	// 77/8000 across an unrelated nearby change, because the probability
	// depends on worker scheduling that any edit can perturb. Checking up
	// front removes the randomness at its source instead of tuning it.
	//
	// Matches the pre-check convention already used in this package by
	// filewriter.go and diskspace.go.
	if err := ctx.Err(); err != nil {
		return err
	}

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

	// Control message convention: FileIdx=fileIdxCancelJob and a non-nil
	// ackCh, with the real job ID in MessageID and JobID left empty. The
	// worker discriminates on ackCh, which is unexported; the sentinel picks
	// which control message this is.
	ack := make(chan error, 1)
	control := WriteRequest{
		JobID:     "",
		FileIdx:   fileIdxCancelJob,
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
	case err := <-ack:
		// A close-time failure on THIS path is not merely informational.
		// maybeFinalize is the only production caller, and it is about to
		// hand the job to par2, unrar and cleanup — over a file whose
		// buffered bytes never reached the platter. It used to be dropped
		// entirely: the arm computed the error and the ack was a bare close,
		// so this returned nil regardless and the only trace was a Warn
		// inside drainAndClose.
		return err
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
// Its error can be a *storagefault.Fault about a FILE, not only a submit or
// timeout error about the call — a caller matching on it has to expect both.
// That reports at least one of the job's files failing its close-time Drain,
// Sync or Close, which matters because the only production caller is
// maybeFinalize: it is about to hand this job to par2, unrar and cleanup, and
// a file whose close-time drain failed has buffered bytes that never reached
// the platter.
func (a *Assembler) CloseJobHandles(ctx context.Context, jobID string) error {
	// A context that is ALREADY cancelled resolves here, before the selects
	// below, and that is a correctness requirement rather than a fast path.
	//
	// Both selects have ctx.Done() ready from the start, and Go picks
	// uniformly at random among ready cases. So the first select had a ~50%
	// chance of enqueueing the control message anyway, and if the worker then
	// acked before the second select was evaluated, that select could pick
	// <-ack and return nil — reporting success for a call the caller had
	// already cancelled, after doing the work it asked not to be done.
	//
	// It is a real race, not a test artifact: TestAssembler_CloseJobHandles_ContextCanceled asserted
	// context.Canceled and failed about 1 run in 20 at package scope under
	// -race. Measured directly at -count=2000, the rate moved from 8/8000 to
	// 77/8000 across an unrelated nearby change, because the probability
	// depends on worker scheduling that any edit can perturb. Checking up
	// front removes the randomness at its source instead of tuning it.
	//
	// Matches the pre-check convention already used in this package by
	// filewriter.go and diskspace.go.
	if err := ctx.Err(); err != nil {
		return err
	}

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

	// Control message convention: FileIdx=fileIdxCloseHandles and a non-nil
	// ackCh, with the real job ID in MessageID and JobID left empty. The
	// worker discriminates on ackCh, which is unexported; the sentinel picks
	// which control message this is.
	ack := make(chan error, 1)
	control := WriteRequest{
		JobID:     "",
		FileIdx:   fileIdxCloseHandles,
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
// barrier is the only component that acks them while the download is running
// (X2). Articles this assembler never saw — those an EARLIER process wrote —
// are resolved by the startup resume sweep instead, with no barrier involved;
// see docs/durability-contract.md §1. The batching that used to
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
				// this path cannot skip drainAndCloseAll and leave cached bytes
				// unwritten with their handles still open.
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
			// WriteRequest has a nil ackCh and a nil syncOp, so it matches no
			// control arm in dispatchRequest and would reach processRequest
			// with an empty job ID.
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
// cancel control messages (a non-nil ackCh with FileIdx==fileIdxCancelJob) by closing and removing
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

	// Control messages are told from articles by an unexported field being
	// set, not by the FileIdx sentinel alone.
	//
	// The sentinel is the encoding; it is not the proof. JobID and FileIdx are
	// both exported and both overwritten from the caller's ArticleRef, so on
	// the sentinel alone a caller outside this package could submit an article
	// with an empty JobID and a negative FileIdx and have the worker act on it
	// as a control message — for the barrier arm, dereferencing a syncOp the
	// caller had no way to set. ackCh and syncOp are unexported, so no value
	// built outside internal/assembler can have either, and the confusion is
	// unrepresentable rather than rejected.
	//
	// A zero WriteRequest is excluded by the same test, which matters for the
	// shutdown drain loop below: it dispatches zero values when the channel is
	// closed, and they must reach processRequest rather than any arm here.
	if req.syncOp != nil {
		// Control message: a barrier operation. Answered on this goroutine,
		// which owns every file handle and every write cache (X1).
		a.handleSyncOp(req.syncOp, open, wc)
		return 0
	}
	if req.ackCh != nil && req.FileIdx == fileIdxCancelJob {
		// Control message: cancel a job. Close and remove all
		// open files for the job encoded in MessageID.
		cancelID := req.MessageID
		cancelledJobs[cancelID] = struct{}{}
		for k, f := range open {
			if k.jobID != cancelID {
				continue
			}
			// The close error is genuinely best-effort here — the file is
			// removed on the next line — but the faulted set is not the same
			// kind of thing, so it is reported rather than discarded silently.
			//
			// Dropped, not routed. The job is being torn down: its files are
			// deleted below, its cache forgotten, and RemoveJob ordinarily
			// removes it from the queue next. Returning these articles to
			// Outstanding would re-dispatch work for a job that is going away.
			//
			// "Ordinarily" is exact rather than hedging. RemoveJob can return
			// early when the queue's store delete fails, leaving the job
			// resident with its files already gone (#376), so the teardown
			// this drop assumes is not unconditional. It holds anyway only
			// because the set is empty — see below — which is why this is a
			// tripwire and not a disposition anyone should rely on.
			//
			// The set is empty on every reachable path — see FileWriter.Close
			// — so this branch is a tripwire, not a handler. If it ever fires,
			// a producer has been added that the worker does not drain.
			leaked, _ := f.w.Close()
			if len(leaked) > 0 {
				// The indices, not just a count. This arm DROPS the set, so
				// this line is the only record left of which articles were
				// stranded — a count names nothing an operator or a bug report
				// could act on.
				a.log.Error("articles were rolled back and never routed before a "+
					"cancelled job's file was closed; they keep their Emitted bit and "+
					"will not be re-dispatched until a restart",
					"job", k.jobID, "fileidx", k.fileIdx,
					"articles", len(leaked), "artidxs", faultedIndices(leaked))
			}
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
	if req.ackCh != nil && req.FileIdx == fileIdxCloseHandles {
		// Control message: close all open file handles for a job without deleting files.
		targetID := req.MessageID
		var closeErr error
		for k, f := range open {
			if k.jobID != targetID {
				continue
			}
			cerr := a.drainAndClose(f)
			if cerr != nil {
				// Recorded on the ack, not swallowed. maybeFinalize is about
				// to hand this job to par2, unrar and cleanup, and a file
				// whose close-time drain failed has buffered bytes that never
				// reached the platter.
				closeErr = errors.Join(closeErr, cerr)
			}
			delete(open, k)
			// Tombstoned unconditionally, including on failure, and that is
			// deliberate. An earlier version of this gated the tombstone on
			// cerr == nil, on the theory that a re-dispatched article should
			// be able to reopen the file. It cannot be re-dispatched — this
			// arm's only production caller is maybeFinalize, which sets
			// PostProc first, and ForEachUnfinishedArticle skips a PostProc
			// job — so the gate bought nothing and cost a great deal: the key
			// was then in NEITHER map, so an article already in flight when
			// the fault hit missed both guards and fell into openTargetFile,
			// which re-creates and re-preallocates a file the job has already
			// handed to post-processing. The fd it opens is never closed
			// (this control message is one-shot behind SetPostProcStarted),
			// so the job reappears in OpenJobIDs, the checkpoint loop
			// barriers a job in post-processing, and on NFS the handle held
			// across the unlink is the silly-rename this function exists to
			// prevent.
			completed[k] = struct{}{}
			// drainFile retains the per-file cache entry to preserve its
			// write cursor, so a drain alone leaves the cache holding a key
			// the open map no longer has. It used to be unreachable here —
			// finalizeFile forgot the entry when it closed the file — but
			// finalizeFile no longer closes, so this branch can now be the
			// first to see a completed file and has to do it itself. Safe
			// after a drain: drainFile cleared the articles map, so forget
			// has nothing left to double-pool.
			wc.forget(k)
		}
		if req.ackCh != nil {
			if closeErr != nil {
				req.ackCh <- closeErr
			}
			close(req.ackCh)
		}
		return 0
	}
	// Skip articles for cancelled jobs.
	if _, cancelled := cancelledJobs[req.JobID]; cancelled {
		if req.Data != nil {
			a.releaseBuffer(req.Data)
		}
		return 0
	}
	a.processRequest(req, open, completed, wc)
	return 1
}

// drainAndClose flushes a file's buffered bytes, fsyncs, closes it, and
// reports whether any of the three failed.
//
// # What happens to the articles the drain WROTE
//
// They are not acked — this package has no ack authority — and no later Drain
// reports them either, because Close throws the writer away and its retained
// report with it.
//
// It is worth being exact about which step does that, because this comment
// used to name the wrong one: it said "the Sync that follows is what discards
// a confirmed one". FileWriter.Sync discards nothing, and says so in its own
// doc — Confirm releases the report, and drainAndClose never calls Confirm. So
// a reader tracing "who acks these?" was sent looking for a Sync-side discard
// that does not exist. It is Close.
//
// Their Emitted bits therefore stay set, which is NOT the same as Outstanding
// — an earlier version of this doc said "left Outstanding and re-fetched (S3)"
// and that was only ever true of the worker-exit caller, where the next start's
// ClearAllEmitted resets them. On the CloseJobHandles path the process keeps
// running and nothing resets them until it stops.
//
// That costs nothing in the ordinary case. A clean stop runs
// Application.shutdownCheckpoint — a full barrier, ack included — while the
// downloader is already stopped and the handles still exist, so by the time
// this runs there is little left to report.
//
// # What happens to the articles the drain ROLLED BACK, which is the fix here
//
// A failed Drain rolls back everything after the failing write into w.faulted,
// and takeFaulted is its only consumer. Without the releaseFaulted below that
// set died with the writer, and those articles kept their Emitted bits: not
// Done, not Failed, not Outstanding.
//
// They no longer keep their place in partsWritten as well — FileWriter.fail
// gives the part back as it rolls the article back — so what is lost by
// skipping the release is the routing alone. That is still the half that
// strands an article for the life of the process.
//
// # Why the fault is REPORTED and not ROUTED
//
// Returning it is the pattern opDrain, opSync and opTruncate already use, and
// the reason to prefer it here is not symmetry. Routing a fault out-of-band
// from this function was tried and is wrong three ways:
//
//   - It bypasses durability.ErrFaultRouted. Barrier.raise mints that marker so
//     the application layer does not park a job twice for one condition; a
//     fault built here carries no marker and reaches Stallable directly, so a
//     wedged mount that already stalled the job through the barrier's own Drain
//     stalls it again when the same file is closed.
//   - On the CloseJobHandles path the job is already StatusVerifying, which is
//     a phase neither Stall nor Fail can act on: Verifying → Paused is not a
//     legal status edge, and maybeFinalize is a no-op once PostProc is set.
//   - On the opClose path the barrier has already drained, synced, truncated,
//     committed the extent and acked the articles. A fault from the redundant
//     second fsync would race the completion it is part of.
//
// A permanent fault is preferred over the first one when they differ, because
// only the permanent one preserves R20. ENOSPC on the drain followed by EROFS
// on the close — an ext4 mounted errors=remount-ro, the Debian default — is
// the case that makes the difference: reporting the ENOSPC alone describes the
// condition as one that waiting can clear, when it cannot.
func (a *Assembler) drainAndClose(f *openFile) error {
	key := f.w.key
	var first, permanent error
	note := func(op string, err error) {
		if err == nil {
			return
		}
		a.log.Warn(op, "path", f.info.Path, "error", err)
		if first == nil {
			first = err
		}
		if permanent != nil {
			return
		}
		if fault, ok := errors.AsType[*storagefault.Fault](err); ok && fault.Permanent {
			permanent = err
		}
	}

	_, err := f.w.Drain()
	note("drain file before close", err)
	// Unconditional, and cheap when there is nothing to give back: this is the
	// only chance to return what the drain rolled back, because Close below
	// throws the writer away and its faulted set with it.
	a.releaseFaulted(f, key.jobID, key.fileIdx)
	note("sync file before close", f.w.Sync())
	// A failing Close is a storage condition too, and on network-backed mounts
	// it is frequently the first report of writes that never landed — the
	// close is where a deferred error surfaces.
	//
	// The faulted set it returns is separate from that error and is expected
	// to be empty: the releaseFaulted above drained it, and neither Sync nor
	// Close can add to it. Routed anyway rather than dropped — unlike the
	// cancel path, this file is being closed normally and its articles are
	// still wanted — and reported at Error, because a non-empty set here means
	// a producer has appeared that nothing drains.
	leaked, cerr := f.w.Close()
	note("close file", cerr)
	if len(leaked) > 0 {
		a.log.Error("articles were rolled back and never routed before the file was "+
			"closed; routing them now, but a producer of the faulted set is not being "+
			"drained",
			"path", f.info.Path, "articles", len(leaked), "artidxs", faultedIndices(leaked))
		a.routeFaulted(leaked, key.jobID, key.fileIdx, f.info.Path)
	}

	if permanent != nil {
		return permanent
	}
	return first
}

// drainAndCloseAll drains and closes every remaining open file. Called on
// worker exit. Completion callbacks do NOT fire for partial files — writing
// N-of-M parts is not a completion event.
func (a *Assembler) drainAndCloseAll(open map[fileKey]*openFile) {
	for _, f := range open {
		// Nowhere to report it. This runs on worker exit, so there is no
		// caller left to answer and no barrier to route through — and routing
		// it out-of-band here is specifically unsafe: Application.Shutdown
		// joins this goroutine through waitBounded, which ABANDONS its step
		// after the budget and returns while the worker runs on. A callback
		// firing after that lands on app.wg.Go concurrently with app.wg.Wait,
		// which is a WaitGroup misuse panic, and it would take the process
		// down before Shutdown's final queue.Save.
		//
		// drainAndClose has already logged each failure and returned the
		// articles it rolled back, and the next start re-derives the job's
		// state from disk regardless (S3).
		_ = a.drainAndClose(f)
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
		a.handleLateDuplicate(open[key], req)
		return
	}

	f, ok := open[key]
	if !ok {
		var err error
		f, err = a.openTargetFile(key, req, open, wc)
		if err != nil {
			// Routed through OnWriteFault, because nothing else can reach
			// it: the file is never inserted into open, so opFiles never
			// lists it, Files() never includes it, and no barrier operation
			// can return it. Dropped, a persistent EACCES or EROFS on the
			// download directory left the job at N% with no reason attached
			// and the article Emitted forever — the outcome openTargetFile's
			// own doc says returning a fault prevents.
			//
			// No path is passed, and none is needed: openTargetFile returns an
			// ALREADY-classified fault carrying the path its own failing
			// syscall targeted, and noteWriteFault keeps that rather than
			// relabelling it. Re-resolving here would call the FileInfo
			// resolver a second time on a path where the resolver is itself
			// the thing that may have failed.
			// Reported alongside the fault rather than left to
			// releaseFaulted: there is no FileWriter to have rolled it back,
			// because the file was never opened. The article is nevertheless
			// un-written and still Emitted, which is not Outstanding.
			a.noteArticlesUnwritten(req.JobID, req.FileIdx, []int32{req.ArtIdx})
			a.noteWriteFault("", req, err)
			if req.Data != nil {
				a.releaseBuffer(req.Data)
			}
			return
		}
	}

	// admitted reports whether the article was taken on as a part of this
	// file. The COUNT is not applied here — it belongs to FileWriter, applied
	// by admitAccepted and admitPermanentFailure in the same breath as the
	// seen-set record it is derived from. A count applied here, after the
	// write, could be rolled back by that write's own failure: w.fail decides
	// the give-back from seenDone membership, so the two were set at different
	// moments and the rollback reversed a count that had not been applied yet.
	// Every transient accept-time write fault undercounted the file by one,
	// permanently, and partsWritten >= TotalParts became unreachable.
	var admitted bool
	if req.FatalErr != nil {
		if admitted = a.handleFatalArticle(f, req); admitted && req.Data != nil {
			a.releaseBuffer(req.Data)
		}
	} else {
		admitted = a.handleSuccessArticle(f, req)
	}
	if admitted {
		// Memory pressure is a whole-cache property, not a per-file one, so
		// it is relieved here rather than inside FileWriter: only the worker
		// can see every file and pick the largest. B2 bounds cached article
		// memory by configuration independently of job size, file size and
		// job count, and nothing else enforces that bound.
		a.relievePressure(wc, open)
	}

	// THE drain, and the reason it is one call on every path rather than one
	// per producer.
	//
	// The invariant is "w.faulted is empty when the file's ROUTING is
	// complete", and a release scattered across each producer does not
	// establish it — it only establishes that each producer eventually gets
	// drained by somebody. Three defects lived in that gap: the seenFailed
	// retry arm returned without draining, so a displaced article's rollback
	// sat pending until an unrelated later failure; relievePressure drained
	// one line before the increment, so a failed flush un-counted the current
	// article and the increment put it straight back; and handleFatalArticle
	// and the rejection branch both reached the comparison below with a stale
	// set pending. In every case the file could reach TotalParts with a part
	// whose bytes were pooled and never written, firing OnFileComplete at
	// 100% reported health over a hole.
	//
	// Two of those three were about the COUNT, and the count no longer depends
	// on this call: FileWriter.fail gives the part back as it rolls the
	// article back, so the comparison below is correct whether or not anything
	// has been drained. What still depends on it is the ROUTING — an article
	// left in w.faulted is neither Done, nor Failed, nor Outstanding — which
	// is why the call stays unconditional rather than becoming best-effort.
	a.releaseFaulted(f, req.JobID, req.FileIdx)
	if !admitted {
		return
	}
	// Read once, so the count the log reports is provably the count the
	// comparison below decided on.
	parts := f.w.parts()
	a.log.Debug("processed part",
		"job", req.JobID, "fileidx", req.FileIdx,
		"part", parts, "total", f.info.TotalParts,
		"offset", req.Offset, "bytes", len(req.Data), "failed", req.FatalErr != nil)
	if f.info.TotalParts > 0 && parts >= f.info.TotalParts {
		a.finalizeFile(f, key, req, completed)
	}
}

// handleLateDuplicate handles articles arriving for a file that is already
// marked completed. f is the file's still-open entry, or nil if it has been
// closed.
//
// The article is not written. The tombstone exists because the barrier may be
// part-way through finalizing this file — draining, syncing, truncating — and
// a write into it there is the race the tombstone was put down to stop.
//
// Ordinarily nothing else is owed either. A file that reached TotalParts is
// one whose articles are all resolved: accepted and awaiting the barrier's
// ack, or permanently failed. Re-asserting anything about those here would be
// this package claiming authority it no longer has.
//
// # The article that is owed something
//
// An article can reach here holding no state at all: absent from seenDone,
// absent from seenFailed, and holding no part. FileWriter.fail puts it in
// exactly that condition — it clears the seenDone entry and gives the part
// back together — for every article a coalesced run or a drain rolled back.
// The rollback returned it to Outstanding, the downloader re-dispatched it,
// and the copy that comes back arrives after its file was tombstoned.
//
// A cache displacement no longer reaches that condition. failDisplaced counts
// the article and records it in seenFailed (#386), so a redelivery of one is
// recognised below rather than falling through to the rejection arm and being
// reported permanently failed a second time.
//
// An earlier version of this paragraph derived the same conclusion from
// "partsWritten is incremented when an article is ACCEPTED and is never
// decremented". That premise was already false when it was written — the
// give-back existed, in releaseFaulted — and it is comprehensively false now.
// The conclusion survives the correction because it never depended on the
// count: what strands the article is the absence of a RECORD, not the presence
// of a part.
//
// Dropping that one strands it: not written, not acked, not failed, and
// Emitted again from the re-dispatch — which ForEachUnfinishedArticle skips,
// so nothing re-dispatches it a second time and the job never finishes.
//
// It is reported as rejected, which resolves it as permanently failed. That is
// the honest disposition rather than a convenient one: the file is finished,
// its content is whatever the barrier recorded, and this package cannot write
// the article now or later. Returning it to Outstanding instead would have it
// re-dispatched into the same tombstone forever. Failing it charges its bytes
// against the job's par2 recovery budget, which is exactly the decision par2
// exists to make.
//
// The distinction is drawn on seenDone, so it is only ever made when the
// answer is positively known. An article the writer accepted is dropped
// silently, as before — failing it would charge good bytes against recovery
// and degrade the job's reported health. Since #386 that set also contains
// articles which were accepted and then displaced: they keep their seenDone
// entry alongside a seenFailed one, so they are dropped here on the first
// test rather than the second. The disposition is the same either way — this
// package cannot write them now — but the reason is no longer only "accepted".
// When the writer is gone there is nothing to consult, and dropping is the
// older, safer behaviour.
func (a *Assembler) handleLateDuplicate(f *openFile, req WriteRequest) {
	a.log.Debug("ignoring late article for completed file",
		"job", req.JobID, "fileidx", req.FileIdx, "msgid", req.MessageID)
	if req.Data != nil {
		a.releaseBuffer(req.Data)
	}
	if f == nil {
		return
	}
	if _, accepted := f.w.seenDone[req.ArtIdx]; accepted {
		return
	}
	if _, failed := f.w.seenFailed[req.ArtIdx]; failed {
		// Already resolved in the other direction by the path that failed it.
		return
	}
	a.log.Warn("late article for a completed file was never accepted; recording it as "+
		"permanently failed so the job is not left waiting on it",
		"job", req.JobID, "fileidx", req.FileIdx, "artidx", req.ArtIdx, "msgid", req.MessageID)
	if a.opts.OnArticleRejected != nil {
		a.opts.OnArticleRejected(req.JobID, req.FileIdx, req.ArtIdx,
			"redelivered after its file was completed, with no record of it having been written")
	}
}

// openTargetFile resolves file information and creates the target file on disk
// for a new fileKey.
//
// Every failure returns a classified *storagefault.Fault rather than logging
// and discarding the article (#357). The distinction matters because of A2: a
// dropped article is resolved neither way, so no later run re-dispatches it and
// the job stalls at 99% forever.
//
// Returning the fault is necessary but not currently sufficient, and an
// earlier version of this comment claimed otherwise ("a returned fault leaves
// it Outstanding, which costs a re-fetch and nothing else"). processRequest
// drops what this returns, and the article keeps its Emitted bit either way —
// see the KNOWN GAP note at the call site.
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
			a.releaseBuffer(req.Data)
		}
		return nil, storagefault.Classify("resolve", "", err)
	}

	dir := filepath.Dir(info.Path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		if req.Data != nil {
			a.releaseBuffer(req.Data)
		}
		return nil, storagefault.Classify("mkdir", info.Path, err)
	}
	//nolint:gosec // G304: path is caller-supplied from FileInfo resolver, which is responsible for safe derivation
	fh, err := os.OpenFile(info.Path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		if req.Data != nil {
			a.releaseBuffer(req.Data)
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
	// early articles are not re-delivered never forms a contiguous run from 0.
	//
	// What that costs is NOT "its articles are written individually", which is
	// what this comment used to say. flushContiguous forms no run at all, so
	// nothing is written on the arrival path: the articles stay buffered in
	// the write cache until a barrier's Drain or a memory-pressure flush
	// pushes them out. So the cost is a fuller cache and writes deferred to
	// the checkpoint, not extra syscalls — and it is still never correctness,
	// because both of those paths write every buffered article.
	f := &openFile{
		w:    newFileWriter(fh, info.Path, key, wc),
		info: info,
	}
	open[key] = f
	return f, nil
}

// handleFatalArticle counts a permanently failed article toward the file's
// part total without writing anything.
//
// It records no ack. A permanent failure is the queue's to record via
// AckPermanentFailure (R10), and this package no longer has an ack path in
// either direction. The dedup that keeps partsWritten from overshooting
// TotalParts is purely local bookkeeping, and it lives on the writer with the
// seen-sets it reads and the counter it moves — see admitPermanentFailure.
//
// Returns false when the article must not be counted again.
func (a *Assembler) handleFatalArticle(f *openFile, req WriteRequest) bool {
	a.log.Debug("counting failed article toward completion (skipping disk write)",
		"job", req.JobID, "fileidx", req.FileIdx, "path", f.info.Path, "error", req.FatalErr)
	return f.w.admitPermanentFailure(req.ArtIdx)
}

// handleSuccessArticle hands one article's bytes to the file's writer.
//
// Returns false when the article must not be counted toward the file's part
// total: a duplicate, a retry of an article already counted as failed, or a
// write the storage layer refused. A REJECTED article does count — it is
// resolved permanently failed, so a file waiting for it waits forever. See the
// accept-failure branch below for both halves.
func (a *Assembler) handleSuccessArticle(f *openFile, req WriteRequest) bool {
	w := f.w
	id := articleID{msgID: req.MessageID, artIdx: req.ArtIdx}
	if _, dup := w.seenDone[req.ArtIdx]; dup {
		// A duplicate of an article already accepted. Its first copy is
		// either still buffered or already written; either way this copy's
		// bytes are redundant, and re-writing them would be a second
		// WriteAt for the same range. Nothing is claimed here — the
		// barrier absorbs duplicate reports itself (R12).
		if req.Data != nil {
			a.releaseBuffer(req.Data)
		}
		return false
	}
	if _, was := w.seenFailed[req.ArtIdx]; was {
		// A retry of an article already counted as failed. The false
		// return keeps partsWritten from counting it twice; the bytes are
		// still written, because they are still the file's content.
		w.admitRetryOfFailed(req.ArtIdx)
		if err := a.acceptArticle(f, id, req); err != nil {
			a.routeAcceptFailure(f, req, err)
		}
		return false
	}
	// Recorded before the write is attempted, not after, so a write path that
	// fails can move the article to seenFailed without this function putting
	// it straight back — and recorded in the same breath as the count, which
	// is what admitAccepted exists to make inseparable.
	w.admitAccepted(req.ArtIdx)
	if err := a.acceptArticle(f, id, req); err != nil {
		// Whether it still counts depends on WHICH failure it was, and that is
		// the A1 split reaching the part total.
		//
		// A storage fault must not count. Counting it is what let a file reach
		// TotalParts and finalize over bytes that never landed, reporting full
		// health on a job whose volume was full. Its article stays Outstanding
		// and arrives again, so the part total is not lost — it is deferred.
		//
		// A REJECTED article must count, and for the mirror reason: it is
		// resolved permanently failed and will never arrive again. Declining
		// to count it means partsWritten can never reach TotalParts, so
		// OnFileComplete never fires, MarkFileComplete never runs, and the job
		// sits at 100% with zero outstanding articles across restarts. This is
		// what handleFatalArticle already does for a permanent failure, which
		// is the same fact arriving from the other side of the pipeline.
		//
		// Counting it claims nothing about its bytes: it decoded no Class A
		// fact and earns no durable bit, so no fact-derived truncate bound
		// reaches past it, and its bytes are charged to failedBytes for par2
		// to repair from.
		return a.routeAcceptFailure(f, req, err)
	}
	return true
}

// acceptArticle range-checks the write and hands it to the file's writer,
// returning any storage fault the write raised.
//
// An earlier version of this doc claimed the fault was "logged and left to the
// barrier, which is what surfaces it to the job via Stallable", and that the
// article "simply does not appear in the next Drain, which leaves it
// Outstanding". Neither half held. The barrier only sees a fault that Drain,
// Sync, Stat or Truncate returns, and a write rejected here leaves nothing
// behind for a later Drain to fail on — with the cache disabled there is
// nothing buffered at all, so no fault was ever routed. Nor is the article
// Outstanding: its Emitted bit is still set, and ForEachUnfinishedArticle
// skips it, so it is never re-dispatched.
//
// Returning the fault is what makes A1 true rather than merely asserted: the
// caller declines to count the part, returns the rolled-back articles through
// OnArticlesUnwritten, hands the fault to OnWriteFault, and the job stalls on a
// storage condition without any article being recorded as damaged.
func (a *Assembler) acceptArticle(f *openFile, id articleID, req WriteRequest) error {
	if reason, ok := a.offsetOutOfRange(f, req); ok {
		if req.Data != nil {
			a.releaseBuffer(req.Data)
		}
		f.w.failPermanent(id)
		return &rejectedArticleError{reason: reason}
	}
	// An offset whose owner has already been reported Written is settled, and
	// the ARRIVING article is the one refused. See FileWriter.offsetSettledBy
	// for why the loser is chosen this way round rather than by arrival order.
	//
	// Checked here rather than inside Accept so the refusal travels the same
	// route as the out-of-range one above — Accept's contract is that its error
	// always reports STORAGE failing, and this reports the article.
	if owner, settled := f.w.offsetSettledBy(req.Offset, id); settled {
		if req.Data != nil {
			a.releaseBuffer(req.Data)
		}
		f.w.failPermanent(id)
		rej := &rejectedArticleError{
			reason: "claims a byte offset already written by another article",
		}
		if f.w.notePostAnomaly() {
			rej.anomaly = postAnomalyReason(f.info.Path, faultedArticle{
				id: id, offset: req.Offset, displacedBy: owner,
			})
		}
		return rej
	}
	return f.w.Accept(id, req.Offset, req.Data)
}

// rejectedArticleError marks a refusal that is about the ARTICLE, so the
// caller can tell it apart from a storage fault and route it to
// OnArticleRejected instead of OnWriteFault (A1).
//
// It returns an error rather than a bool because acceptArticle's other failure
// is already an error, and a rejection that returned nil was counted as a
// successful part — see handleSuccessArticle.
type rejectedArticleError struct {
	reason string

	// anomaly, when non-empty, is a job-level description of a structural
	// fault in what the servers served — currently only an offset collision.
	//
	// It rides on the rejection because the two are one event seen at two
	// scales: reason resolves THIS article, anomaly explains the shape of the
	// fault once per file. Empty for an out-of-range offset, which is a
	// property of one article rather than of the post.
	anomaly string
}

func (e *rejectedArticleError) Error() string {
	return "assembler: article rejected: " + e.reason
}

// routeAcceptFailure sends one acceptArticle failure to the callback that
// matches what actually failed, which is the A1 split in one place.
//
// It reports whether the article still counts toward the file's part total.
// True for a rejection — the article is resolved and will never arrive again,
// so a file waiting for it waits forever — and false for a storage fault,
// whose article stays Outstanding and arrives again. See handleSuccessArticle,
// which is where the consequence of each is written out.
func (a *Assembler) routeAcceptFailure(f *openFile, req WriteRequest, err error) bool {
	if rej, ok := errors.AsType[*rejectedArticleError](err); ok {
		a.log.Warn("article rejected; it will be recorded as permanently failed",
			"job", req.JobID, "fileidx", req.FileIdx, "artidx", req.ArtIdx,
			"path", f.info.Path, "reason", rej.reason)
		if a.opts.OnArticleRejected != nil {
			a.opts.OnArticleRejected(req.JobID, req.FileIdx, req.ArtIdx, rej.reason)
		}
		// Paired with the per-article report above, not an alternative to it.
		if rej.anomaly != "" && a.opts.OnPostAnomaly != nil {
			a.opts.OnPostAnomaly(req.JobID, req.FileIdx, rej.anomaly)
		}
		return true
	}
	// Released here as well as by processRequest's drain. This one is not
	// redundant belt-and-braces: it keeps the roll-back adjacent to the
	// failure that caused it, which is what the unit-level contract for this
	// function asserts. The drain is what makes the INVARIANT hold for
	// producers that have no such site.
	a.releaseFaulted(f, req.JobID, req.FileIdx)
	a.noteWriteFault(f.info.Path, req, err)
	return false
}

// releaseFaulted returns every article a failed writer operation rolled back
// to Outstanding, and reports the displaced ones as permanently failed.
//
// Called after every operation that can populate w.faulted — which is not the
// same set as "every operation that can fail", and reading it as such is how
// two producers went unpaired. It includes the ones whose fault the barrier
// routes rather than this package (the two are separate concerns: the barrier
// can park the job, but it never learns which articles lost their bytes,
// because that set does not cross the SyncTarget interface), the close-time
// drain, and Accept's displaced-article loop, which resolves an article on a
// path where nothing failed at all.
//
// Counting is NOT part of this, in either direction. FileWriter.fail gives the
// part back as it rolls the article back, in the same statement pair that
// removes it from seenDone, and failDisplaced counts the displaced article
// through admitPermanentFailure rather than giving anything back — so by the
// time the set arrives here every count is already correct. This function's
// whole remaining job is disposition: back to Outstanding, or resolved
// permanently failed.
//
// It used to do both, driven by a stored uncount flag on each faultedArticle.
// That flag was derived from seen-set membership the writer owned and applied
// by code that did not, which is the arrangement every count defect in this
// area came out of.
func (a *Assembler) releaseFaulted(f *openFile, jobID string, fileIdx int) {
	a.routeFaulted(f.w.takeFaulted(), jobID, fileIdx, f.info.Path)
}

// routeFaulted disposes of one already-taken set of rolled-back articles.
//
// Split from releaseFaulted so Close's return value has somewhere to go. It
// takes no *openFile deliberately: there is nothing left for it to do to the
// file, because every part was settled when the articles were rolled back or
// resolved.
// Anything it could reach for would be state it does not own.
func (a *Assembler) routeFaulted(rolled []faultedArticle, jobID string, fileIdx int, path string) {
	if len(rolled) == 0 {
		return
	}
	arts := make([]int32, 0, len(rolled))
	for _, r := range rolled {
		if r.displaced {
			// Resolved, not re-fetched. Returning it to Outstanding produced a
			// ping-pong: the re-fetched copy displaces the article that
			// displaced it, which is then released in turn. See failDisplaced.
			a.log.Warn("article was displaced by another claiming the same offset; "+
				"recording it as permanently failed",
				"job", jobID, "fileidx", fileIdx, "artidx", r.id.artIdx,
				"offset", r.offset, "displacedby", r.displacedBy.artIdx)
			if a.opts.OnArticleRejected != nil {
				a.opts.OnArticleRejected(jobID, fileIdx, r.id.artIdx,
					"displaced by a later article claiming the same offset")
			}
			// Once per file, alongside the per-article report above.
			if r.firstCollision && a.opts.OnPostAnomaly != nil {
				a.opts.OnPostAnomaly(jobID, fileIdx, postAnomalyReason(path, r))
			}
			continue
		}
		arts = append(arts, r.id.artIdx)
	}
	a.noteArticlesUnwritten(jobID, fileIdx, arts)
}

// postAnomalyReason describes an offset collision for a human, without
// diagnosing it.
//
// Every clause is an observation. "Claim the same byte range" is what the
// articles' own =ypart begin= values say; it does not assert the post is
// malformed, because a redundant posting and a server that mangled the digits
// in transit produce the identical observation and nothing here can tell them
// apart. See Options.OnPostAnomaly.
//
// It names the file rather than the file index because the index means nothing
// to a user reading a queue row, and both Message-IDs because they are what
// makes the claim checkable against the NZB.
//
// The wording must not CONTAIN WarningsBanner.svelte's 'Duplicate NZB': that
// banner counts duplicates by substring, so a reason merely containing the
// phrase would be tallied as one.
func postAnomalyReason(path string, r faultedArticle) string {
	return fmt.Sprintf(
		"Overlapping segments in %s: %s and %s both claim byte offset %d, so only "+
			"one of them can be written. %s was discarded and this file may be "+
			"incomplete.",
		filepath.Base(path), articleLabel(r.displacedBy), articleLabel(r.id),
		r.offset, articleLabel(r.id))
}

// articleLabel identifies an article for a human. Not every article has a
// Message-ID — the NZB may omit it — so the index is the fallback rather than
// printing an empty string where an identifier belongs.
func articleLabel(id articleID) string {
	if id.msgID == "" {
		return fmt.Sprintf("#%d", id.artIdx)
	}
	return "<" + id.msgID + ">"
}

// faultedIndices lists the article indices in a rolled-back set, for the two
// tripwire logs at the Close call sites.
//
// It exists because those two lines used to report a COUNT. On the cancel path
// the set is dropped, so the log is the only record that survives it, and
// "articles=3" names nothing anyone could act on — not the articles, and not
// the producer that must have been added for the set to be non-empty at all.
// The disposition is unchanged either way; this only makes the report legible.
func faultedIndices(rolled []faultedArticle) []int32 {
	out := make([]int32, len(rolled))
	for i, r := range rolled {
		out[i] = r.id.artIdx
	}
	return out
}

// noteArticlesUnwritten hands one set of un-written articles to their owner.
func (a *Assembler) noteArticlesUnwritten(jobID string, fileIdx int, arts []int32) {
	if len(arts) == 0 {
		return
	}
	a.log.Warn("articles did not reach disk; they return to Outstanding",
		"job", jobID, "fileidx", fileIdx, "articles", len(arts))
	if a.opts.OnArticlesUnwritten != nil {
		a.opts.OnArticlesUnwritten(jobID, fileIdx, arts)
	}
}

// noteWriteFault surfaces a failed write to the owner of the job.
//
// Classify is applied only when the writer did not already return a typed
// fault, so a fault that arrived with its own op and path keeps them rather
// than being relabelled "write" against this file.
func (a *Assembler) noteWriteFault(path string, req WriteRequest, err error) {
	fault, ok := errors.AsType[*storagefault.Fault](err)
	if !ok {
		fault = storagefault.Classify("write", path, err)
	}
	a.log.Error("article could not be written",
		"job", req.JobID, "path", path, "offset", req.Offset, "error", err)
	if a.opts.OnWriteFault != nil {
		a.opts.OnWriteFault(req.JobID, req.FileIdx, fault)
	}
}

// finalizeFile records that a file's parts have all arrived and reports it.
//
// It no longer truncates and no longer computes a CRC. Both decisions moved to
// durability.Barrier: the truncate bound is the highest end offset among the
// file's DURABLE articles, which only the fact log knows, and the whole-file
// CRC is FileExtent.PrefixCRC guarded by HasPrefixCRC. Doing either here would
// be this package asserting something about bytes on disk that it cannot check
// — the shape behind #342, #349 and #350.
//
// It no longer CLOSES the file either, and that is the load-bearing part. The
// barrier's FinalizeFile must Drain, Sync, Truncate and Stat this file, and
// every one of those goes through the handle this package owns. Closing here
// would leave the completed file untrimmed — pre-allocation's trailing zeros
// intact, which par2 reports as damage — and would discard the last drain's
// Written articles with nothing to ack them, so they would be re-fetched on
// every restart forever.
//
// So the handle stays open and the file stays in `open` until the caller asks
// for it back via CloseFile, which the completion consumer calls after the
// barrier has finalized the file. The tombstone below is set NOW rather than at
// close, so a late duplicate is rejected during that window instead of being
// written into a file the barrier is finalizing.
//
// The cost is file descriptors, and it is worth stating the bound accurately
// because the obvious one is wrong. A completed file holds its handle until the
// completion consumer works through to it. That consumer is serial and its
// event channel is bounded — but the channel is not the bound: the producer
// side spawns a goroutine per event when the channel is full, precisely so the
// worker here never blocks on a consumer that may need the worker. So the real
// bound is the number of files an active job can complete before the consumer
// catches up, which in the limit is every file of every active job.
//
// That is the same ORDER as the pre-existing worst case — a job's files are
// largely open at once while they are being written, since the dispatcher fans
// out across them — but it now persists past completion rather than ending at
// it, and it grows when finalization is slow (a barrier per file) rather than
// when writing is.
//
// Blocking here instead is not available: OnFileComplete runs on the worker,
// and the consumer's finalize path submits control messages back to this same
// worker, so making the worker wait for the consumer deadlocks both.
//
// Every path that can abandon the handoff (CancelJob, CloseJobHandles, worker
// exit) still closes whatever is left in `open`.
func (a *Assembler) finalizeFile(f *openFile, key fileKey, req WriteRequest, completed map[fileKey]struct{}) {
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

// offsetOutOfRange reports whether a write request's target range is
// implausible for the file it claims to belong to, and why.
//
// req.Offset originates from the yEnc `=ypart begin=` header, which is parsed
// as an unbounded int64 from the article body returned by the NNTP server. It
// is therefore attacker-controlled: without this check, a hostile or
// compromised server can return a single article whose offset makes WriteAt
// produce a file of arbitrary apparent size. The completion truncate no longer
// commits that size — it is bounded by the durable facts — but the sparse file
// itself is still the attack, so the offset is rejected before the write.
//
// It returns the reason rather than a bare bool because the rejection has to
// reach the queue: the article is refused here, and nothing else in the system
// will refuse it again, so a caller that only knew "not written" could neither
// record it failed nor say why.
func (a *Assembler) offsetOutOfRange(f *openFile, req WriteRequest) (string, bool) {
	reject := func(reason string) (string, bool) {
		a.log.Warn("rejecting out-of-range article write offset",
			"path", f.info.Path,
			"offset", req.Offset,
			"bytes", len(req.Data),
			"expected_size", f.info.ExpectedSize,
			"reason", reason,
		)
		telemetry.PipelineErrors.Add(telemetry.ErrClassDiskWriteError, 1)
		return reason, true
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
		return "", false
	}
	// Guard the slack arithmetic itself against a degenerate ExpectedSize.
	if f.info.ExpectedSize > math.MaxInt64-(f.info.ExpectedSize/offsetSlackDivisor) {
		return "", false
	}
	if limit := f.info.ExpectedSize + f.info.ExpectedSize/offsetSlackDivisor; end > limit {
		return reject("write extends past declared file size")
	}
	return "", false
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
					a.releaseBuffer(art.data)
				}
			}
			continue
		}
		for i, art := range arts {
			if err := f.w.writeOne(art); err != nil {
				// writeOne pooled the article it just handled. Everything
				// after it was never attempted and still holds a pooled
				// buffer, so release those or the decoder's pool leaks one
				// per article — the same rule Drain's failure path follows.
				for _, rest := range arts[i+1:] {
					if rest.data != nil {
						a.releaseBuffer(rest.data)
					}
					f.w.fail(rest.id)
				}
				// Every article this flush rolled back, not just the one
				// the write failed on. The rest were never attempted and
				// their buffers are gone, so they are just as un-written.
				//
				// This call used to carry the decrements as well, and ran one
				// line before processRequest's increment — so it decremented
				// for the article being processed, which had not been counted
				// yet, and the increment put it straight back, leaving the
				// file a part ahead of its bytes and able to fire
				// OnFileComplete over a hole. The decrements moved into
				// w.fail above, which applies each one against the seenDone
				// entry it is derived from, so no ordering between this call
				// and any increment can reproduce it.
				a.releaseFaulted(f, key.jobID, key.fileIdx)
				// Routed, and the flush stops. Logging each failure and
				// carrying on wrote every remaining article into the same
				// full device, and nothing ever reached Stallable: the job
				// was not paused on ENOSPC, no reason was surfaced, and a
				// permanent condition never reached Fail.
				a.noteWriteFault(f.info.Path, WriteRequest{
					JobID: key.jobID, FileIdx: key.fileIdx, ArtIdx: art.id.artIdx,
					Offset: art.offset,
				}, err)
				return
			}
		}
	}
}
