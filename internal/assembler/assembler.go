package assembler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
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

	// ErrJobClosed is what Options.FileInfo returns, bare or wrapped, for a
	// job that no longer accepts writes: the worker drops the article it was
	// resolving for, as it would an article for a cancelled job, instead of
	// opening a file. See processRequest.
	ErrJobClosed = errors.New("assembler: job no longer accepts writes")
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
	// negative FileIdx sentinels (see control.go) with JobID empty and the
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
	// job ID on a control message. On the article path the assembler only
	// logs it; it marks nothing Done or Failed in the queue (handleFatalArticle
	// says why). See the block above.
	MessageID string

	// Offset is the byte position within the target file where Data should
	// be written. The caller (decoder) derives this from the article's
	// yBegin/yPart headers.
	Offset int64

	// Data is the decoded article payload. The assembler takes ownership;
	// callers must not modify Data after enqueueing.
	Data []byte

	// CRC32 is the decoded article's CRC32 — the same value the decoder
	// validated against the yEnc trailer. It travels alongside Data to
	// OnArticleWritten.
	CRC32 uint32

	// Release, when non-nil, is called on the worker once it has dispatched
	// the request, whatever became of it: written, refused, faulted, or
	// discarded for a cancelled job. A request still queued when the worker
	// exits is not released. A WriteArticle that returns an error
	// never calls it, so the caller still owns what it releases. The pipeline
	// sets it to give the article's bytes back to the downloader's budget.
	Release func()

	// FatalErr is set if the article permanently failed to download.
	// If non-nil, the assembler skips writing and counts the part toward
	// file completion. Duplicate failures are deduplicated locally
	// (per-file seen-set) so partsWritten does not overshoot TotalParts.
	FatalErr error

	// ackCh, when non-nil, is closed by the worker immediately after this
	// control message has been fully processed -- for a cancel, after the
	// job's open file handles have been closed and its files disposed of.
	// Never used by ordinary write requests. Set by the four control-message
	// senders and nothing else: `grep -n 'ackCh:' internal/assembler/*.go |
	// grep -v '_test.go' | grep -v '// '` returns 4 lines, in CancelJob,
	// CloseJobHandles, ForgetJob and Quiesce.
	//
	// It carries an error for the control messages that can fail. A sender
	// with nothing to report closes it without sending, so a receiver reading
	// `<-ackCh` gets a nil error either way — which is what lets an arm with
	// nothing to report stay a bare close.
	ackCh chan error

	// disposition says what the cancel arm does with the files it closes.
	// Ignored everywhere else: `grep -n 'req\.disposition'
	// internal/assembler/*.go | grep -v '// '` returns exactly one read, in
	// the fileIdxCancelJob arm of dispatchRequest, which passes it to
	// closeCancelledFile. The second filter drops a prose mention of the name
	// in dispatchRequest's own comment block.
	//
	// Unexported for the reason the block above dispatchRequest gives for
	// ackCh: JobID and FileIdx are exported and overwritten from
	// the caller's ArticleRef, so a sentinel alone is an encoding rather than
	// a proof. A caller outside this package cannot set this field, so it
	// cannot direct the worker to unlink a file.
	//
	// The zero value is KeepFiles, so a control message that failed to say
	// what it wanted destroys nothing.
	disposition FileDisposition
}

// FileInfo describes a target file. The assembler requests it from the caller's
// resolver the first time it encounters a (JobID, FileIdx) pair.
type FileInfo struct {
	// Dir is the job directory the file belongs in, and Name the file's
	// name inside it. openTargetFile creates Dir and then opens Name with
	// fsutil.OpenNoFollow on an os.Root on Dir. A job file is always a regular
	// file with one link; any link in its place, or a Name that is not one
	// path component, is refused by the open rather than followed.
	Dir, Name string

	// TotalParts is the number of manifest segments in this file. Each segment
	// counts toward it once, when it is accounted for: accepted, or resolved
	// permanently failed (which includes the loser of two segments claiming one
	// offset). A repeated ArtIdx is not counted again. When the count reaches
	// TotalParts, the assembler finishes and closes the file (finalizeFile),
	// and fires OnFileComplete only if both succeeded.
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
	// otherwise remain as trailing zeros, which par2 reports as damage. That
	// trim (fileWriter.finish) does not read this field; it is bounded by the
	// writer's owned ranges. Its readers in this package are openTargetFile, which
	// preallocates to it, and offsetOutOfRange, whose limit depends on this
	// being the larger of the two figures.
	ExpectedSize int64

	// Owned lists byte ranges verified before this process started. An arrival
	// intersecting one is refused, as it would be had this process written it.
	// It is read once, when the file is opened (seedOwned), and not kept: the
	// writer's own range set is the only copy afterwards.
	Owned []Range
}

// Path is the file's path, Dir joined with Name, for messages and for readers
// that open the finished file by path.
func (fi FileInfo) Path() string { return filepath.Join(fi.Dir, fi.Name) }

// WholeArticle is OnArticleRejected's uncovered for a refusal that leaves none
// of the article's bytes on disk: the caller charges the article's full size.
// It is negative so that no byte count can be mistaken for it.
const WholeArticle int64 = -1

// Options configures an Assembler.
type Options struct {
	// QueueSize is the capacity of the internal write-request channel.
	// Zero selects the default (2048).
	QueueSize int

	// FileInfo is called once per (JobID, FileIdx) pair to obtain the target
	// directory, name and expected part count. It must be non-nil; New panics
	// otherwise.
	//
	// It may return ErrJobClosed, bare or wrapped, for a job that no longer
	// accepts writes; the worker then drops the article silently. Any other
	// error is a "resolve" storage fault, routed through OnWriteFault.
	FileInfo func(jobID string, fileIdx int) (FileInfo, error)

	// OnFileComplete, if non-nil, is called on the worker goroutine when all
	// TotalParts for a file have been accounted for and the file has been
	// finished on its writing handle: fsynced, trimmed to its last owned byte,
	// fsynced again and closed (finalizeFile). A file whose finish failed is
	// not reported here; see OnFileUntrusted.
	//
	// It reports no whole-file CRC: the caller derives that from the file's
	// written rows.
	//
	// The callback should be cheap; expensive work should be dispatched
	// asynchronously.
	OnFileComplete func(jobID string, fileIdx int)

	// OnFileUntrusted, if non-nil, is called on the worker goroutine for a
	// file whose bytes this package can no longer vouch for: its completion
	// finish failed, or its fsync or close failed when its handle was closed
	// at the hand-over to post-processing or at worker exit. The caller
	// returns the file's written articles to Outstanding and removes their
	// record. The fault itself, where there is a caller to answer, goes
	// separately to OnWriteFault or the close-handles ack.
	OnFileUntrusted func(jobID string, fileIdx int)

	// SyncFile, if non-nil, replaces (*os.File).Sync for every fsync this
	// package runs on a file it writes. It is a seam for tests to inject a
	// device error; production leaves it nil.
	SyncFile func(*os.File) error

	// OnLowDisk, if non-nil, is called when free space on the target
	// filesystem falls below MinFreeBytes, estimated from a cached statfs
	// reading less the bytes written since (DiskProbe.NoteWritten). It is
	// called on the worker goroutine and should not block for long, and again
	// at every later check while free space stays below the floor.
	OnLowDisk func(dir string, free int64)

	// OnWriteFault, if non-nil, is called on the worker goroutine when a
	// write for an article fails, when the file an article targets cannot be
	// opened, or when a completed file's finish or close fails (finalizeFile).
	//
	// It exists because a fault raised inside fileWriter.accept has no other
	// way out: a rejected write leaves nothing behind for a later fsync to
	// fail on. The article is not failed by this: a storage fault says nothing
	// about the article's availability (A1), so the caller stalls the job and
	// returns the article to Outstanding.
	//
	// It carries NO article index, and that separation is the fix for a whole
	// class of stranding. The signature used to name one article, so a failure
	// that rolled back several could report only whichever article triggered
	// it, and the rest were rolled back silently. Returning articles to
	// Outstanding is OnArticlesUnwritten's job, which takes a set.
	OnWriteFault func(jobID string, fileIdx int, f *storagefault.Fault)

	// OnArticlesUnwritten, if non-nil, is called on the worker goroutine with
	// articles whose bytes did not reach disk: one whose write faulted, or one
	// whose file could not be opened. The caller returns them to Outstanding
	// by clearing their Emitted bits.
	//
	// It is not how a failed fsync returns articles. Those were written, so
	// they are Done rather than Emitted, and OnFileUntrusted returns them.
	//
	// It is separate from OnWriteFault, which carries the fault and no
	// article, so that what the fault rolled back is never inferred from the
	// fault itself.
	//
	// Emitted is NOT Outstanding: ForEachUnfinishedArticle skips a set Emitted
	// bit, so an article reported by neither is stranded for the life of the
	// process — not Done, not Failed, not Outstanding — until something clears
	// the bit. Two things do, and neither is on this path:
	//
	//   - A restart, by NOT persisting the bit rather than by clearing it.
	//     JobProgress is not serialized (internal/job/progress.go), so a job
	//     reloaded from the store starts with none set. Nothing has to run for
	//     this to hold.
	//   - A downloader reload, which calls Job.ClearEmittedForReload(false)
	//     per job and clears them in-process.
	//
	// So "stranded until the process stops" is the worst case, not the only
	// one: a reload in between recovers it.
	//
	// No fault is passed, deliberately. This says nothing about why the write
	// failed and must not be read as evidence about any article (A1).
	OnArticlesUnwritten func(jobID string, fileIdx int, artIdxs []int32)

	// OnArticleRejected reports an article this package refused to write
	// because the article itself is not usable: an out-of-range yEnc offset
	// (offsetOutOfRange), a byte range intersecting one another article
	// already wrote (acceptArticle), or a redelivery after its file completed
	// (handleLateDuplicate).
	//
	// uncovered is how many of the article's decoded bytes the refusal leaves
	// missing from the file, which is what the caller charges against par2.
	// For an intersecting range it is the part no other article's range
	// covers (ownedRanges.covered), often zero (#432); for the other two it
	// is WholeArticle, as nothing of the article is on disk.
	//
	// Charging only the uncovered part does not undercount when the covering
	// article is later lost: an untrusted file's articles return to
	// Outstanding (OnFileUntrusted), so the covering article is fetched again,
	// and its outcome accounts for its range: written again, or failed and
	// charged itself. The refetch lands on a fresh writer: each untrust closes
	// the file's writer first — `git grep -n 'a\.noteFileUntrusted(' --
	// 'internal/assembler/*.go' ':!*_test.go'` returns 3 lines, in
	// finalizeFile and syncAndClose's two callers.
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
	// so the caller records it as permanently failed in the job. That is what
	// charges its bytes against the job's par2 recovery budget. Without it the article stays Emitted, which
	// is not Outstanding: ForEachUnfinishedArticle skips a set Emitted bit, so
	// the job waits forever on an article nothing will re-dispatch.
	OnArticleRejected func(jobID string, fileIdx int, artIdx int32, reason string, uncovered int64)

	// OnArticleWritten, if non-nil, is called on the worker goroutine once per
	// article whose bytes were written without error: off and n are the range,
	// crc the article's CRC32. It is not called for a refused article or a
	// faulted write. A zero-length article is reported with n == 0 but claims
	// no range (ownedRanges.claim); its row (length 0, CRC 0) is verified
	// across a restart without reading the file (docs/durability-contract.md
	// §5).
	//
	// It reports a write, not durability: a later failed fsync of the file
	// untrusts it (OnFileUntrusted), which returns the article to Outstanding;
	// a redelivery opens a fresh writer and calls this again with the same
	// artIdx.
	OnArticleWritten func(jobID string, fileIdx int, artIdx int32, off, n int64, crc uint32)

	// MinFreeBytes is the low-disk threshold. Zero disables disk-space checks.
	MinFreeBytes int64

	// DiskCheckTimeout bounds each FreeBytes call in checkDiskSpace.
	// Zero selects the default (5 seconds).
	DiskCheckTimeout time.Duration
}

// fileKey uniquely identifies a target file within the assembler.
type fileKey struct {
	jobID   string
	fileIdx int
}

// openFile tracks an in-progress file being assembled.
//
// It is now bookkeeping only: the file's handle and every byte that
// moves belong to its fileWriter. What is left here is what the WORKER needs
// to route a request — where the file's writer is, and what it was told about
// the file.
//
// maxWritten, crcParts and crcValid are gone. Each was a fact the assembler
// maintained about bytes on disk in order to truncate or to report a CRC. The
// truncate moved to fileWriter.finish, bounded by the writer's owned ranges,
// and the CRC to the written rows (job.Job.SettleFileCRC). Keeping a shadow
// copy here is exactly the second-writer shape S5 forbids.
//
// partsWritten went the same way, for the same reason at a smaller scale. It
// is derived from the writer's seenDone and seenFailed sets, and holding it
// here made every mutation a synchronisation obligation between two structs
// that nothing enforced. It now lives on fileWriter and is read through
// parts().
type openFile struct {
	w    *fileWriter
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

// FileDisposition says what CancelJob does with the files it closes.
//
// It is a named type rather than a bare bool because the call sites read
// better for it: `CancelJob(ctx, id, KeepFiles)` says at the call what
// `CancelJob(ctx, id, false)` would have made a reader open this file to
// learn. The distinction is the whole subject of #433, where a flag that did
// not say what it meant deleted files a caller had asked to keep.
type FileDisposition bool

const (
	// KeepFiles closes the job's handles and leaves its files on disk. It is
	// the zero value deliberately: the non-destructive outcome is the one a
	// value that never got set should get.
	//
	// What survives is a partial: preallocated to the file's expected size
	// with holes where articles never arrived, and — since the caller that
	// wants this has also removed the job from the queue — with no manifest
	// or durable-run record left that could interpret it. That is the
	// documented meaning of asking to keep a removed job's bytes, not an
	// oversight.
	KeepFiles FileDisposition = false
	// DeleteFiles unlinks each file as it is closed.
	DeleteFiles FileDisposition = true
)

// CancelJob sends a control message to the worker goroutine to close all
// open file handles for the given job, and blocks until the worker has
// actually done so. This prevents FD leaks when a job is removed from the
// queue while articles are still being assembled.
//
// With DeleteFiles it also unlinks them, which lets callers safely delete the
// job's directory immediately after CancelJob returns without racing the
// worker's Close()+Remove() of files still inside it (which on NFS-mounted
// directories produces .nfsXXXXXX silly-rename artifacts and a directory the
// caller's delete can't remove). With KeepFiles the handles are still closed
// before this returns, so a caller that deletes the directory anyway is no
// worse off — it is only the unlink that the disposition suppresses.
func (a *Assembler) CancelJob(ctx context.Context, jobID string, disposition FileDisposition) error {
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
	// diskspace.go's freeBytes and WriteSpeedMBPerSec.
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
		JobID:       "",
		FileIdx:     fileIdxCancelJob,
		MessageID:   jobID,
		ackCh:       ack,
		disposition: disposition,
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
		// The cancel arm acks with a bare close, so this is nil today. It is
		// read as an error rather than a signal because the arm is one of
		// several that share ackCh's contract, and because a future arm that
		// does report would otherwise be silently dropped here.
		//
		// A failure to close a kept file is deliberately NOT reported this way:
		// closeCancelledFile only logs it. RemoveJob — the only production
		// caller of this method — treats a non-nil return as "the handles are
		// not confirmed closed", which is true of a context cancellation and
		// false of a failed close, whose handle is released regardless. Routing
		// one into the other would make the caller's warning say something
		// untrue about FD state, which is the thing that ordering actually
		// depends on.
		//
		// If a caller ever needs to know, the fix is a distinct signal, not
		// this one.
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
// enters post-processing, ensuring no open handles remain that would trigger
// NFS silly-rename (.nfs*) leaks when post-processing unlinks files. The worker
// also tombstones the whole job, so no later article for it is written until
// ForgetJob.
// Its error can be a *storagefault.Fault about a FILE, not only a submit or
// timeout error about the call — a caller matching on it has to expect both.
// That reports at least one of the job's files failing its close-time Sync
// or Close: a file whose close-time Sync or Close failed has written bytes
// that may never have reached the platter. The production caller, app's enqueuePostProc,
// fails the post-processing run on any fault this returns, permanent or
// retryable, and runs the stages after any other error — a timeout with no
// fault observed.
// `git grep -n 'closeJobHandles := app\.assembler\.CloseJobHandles' -- '*.go' ':!*_test.go'`
// finds 1 line, that caller.
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
	// diskspace.go's freeBytes and WriteSpeedMBPerSec.
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
	case err := <-ack:
		// Captured, not discarded. The arm computes closeErr from every
		// syncAndClose it performs and sends it here precisely so this
		// returns it; reading `<-ack` and returning nil made the send side
		// dead code and handed enqueuePostProc a job whose unsynced bytes never
		// reached the platter, with only a Warn inside syncAndClose as a
		// trace. That is the defect this arm's own tombstone comment describes
		// as fixed — it was fixed on the send side only.
		//
		// Not covered end-to-end: faulting a file that a STARTED assembler
		// owns needs the *openFile, and no seam reaches it — every
		// fault-injection test in this package drives dispatchRequest
		// directly. TestCloseJobHandles_ArmSendsTheCloseTimeFaultOnTheAck pins
		// the send; this line is the receive. Verified by mutation: reverting
		// it to `return nil` leaves internal/assembler, internal/app and
		// internal/durability all green, which is also why the original defect
		// survived.
		return err
	case <-a.stopCh:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ForgetJob drops every tombstone this assembler holds for jobID, so its files
// can be written again. A retry returns under the SAME job ID, and per-job
// state latched during the previous attempt would otherwise silently apply to
// the new one.
//
// # What it is for
//
// finalizeFile tombstones a file the moment it reaches TotalParts, and the
// cancel and close-handles arms tombstone the whole job, dropping its per-file
// entries as they do. Nothing removes the job-level tombstone but this: it
// lives as long as the worker goroutine. That is right while the instance is
// still in flight — the tombstones are what stop a late article racing a
// file's completion or the hand-off — and wrong the moment the same job ID
// comes back.
//
// Without this, an in-process retry's articles are dropped unwritten and failed
// again, and a restart is the only thing that clears it, because the maps die
// with the process. Which tombstone drops them depends on how the previous
// attempt ended. For a job the cancel or close-handles arm reached it is
// cancelledJobs, below. For one neither arm reached — CloseJobHandles returns
// ctx.Err() without sending its message when its timeout fires first — it is
// the entry finalizeFile set for each finished file, which sends that file's
// writes to handleLateDuplicate: the buffer is pooled and nothing is written.
// A file that finalized
// SHORT is the case that matters most — Job.ResetForRetry clears its Complete
// flag precisely so the articles it is missing can be re-fetched, and this is
// what lets those articles land.
//
// cancelledJobs is dropped too. An article for a job the cancel or close-handles
// arm tombstoned is discarded before it reaches processRequest, so a retry of
// such a job would otherwise have every article silently dropped rather than
// written, for files never finished as well as finished ones.
//
// Open handles are deliberately left alone. This says nothing about a file
// being written right now; it only forgets that one was finished earlier.
func (a *Assembler) ForgetJob(ctx context.Context, jobID string) error {
	// Checked up front for the reason CancelJob's own doc gives at length: a
	// context already cancelled must not be able to enqueue the control
	// message and then report success because the ack happened to win a
	// uniform-random select.
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

	// Control message convention: FileIdx=fileIdxForgetJob and a non-nil
	// ackCh, with the real job ID in MessageID and JobID left empty. The
	// worker discriminates on ackCh, which is unexported; the sentinel picks
	// which control message this is.
	ack := make(chan error, 1)
	control := WriteRequest{
		JobID:     "",
		FileIdx:   fileIdxForgetJob,
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
	case err := <-ack:
		// The forget arm acks with a bare close, so err is nil; it is read as
		// an error to match ackCh's contract.
		return err
	case <-a.stopCh:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Quiesce returns once every request queued before it has been processed:
// each article ahead of it has been written or refused, and its callbacks
// (OnArticleWritten, OnArticleRejected, OnFileComplete) have run. A downloader
// reload calls it after stopping the old downloader, so that an Emitted bit it
// clears afterwards covers only an article whose bytes never reached pwrite.
//
// Requests enqueued after it are not waited for.
func (a *Assembler) Quiesce(ctx context.Context) error {
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

	ack := make(chan error, 1)
	control := WriteRequest{FileIdx: fileIdxQuiesce, ackCh: ack}
	select {
	case a.reqs <- control:
	case <-a.stopCh:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-ack:
		// The quiesce arm acks with a bare close, so err is nil; it is read as
		// an error to match ackCh's contract.
		return err
	case <-a.stopCh:
		return ErrStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// worker is the single goroutine that owns all file handles and performs disk
// I/O. It runs until stopCh is closed and the request channel is drained.
//
// It mutates no queue state itself. Each article it writes is reported
// through Options.OnArticleWritten once its WriteAt returns, and the caller's
// handler marks it Done. Articles this assembler never saw — those an EARLIER
// process wrote — are resolved by a restart's verification of the written
// rows instead. The batching that used to live here — pending Done/Failed
// maps flushed on a ticker — is gone with the acks: there is nothing left to
// batch.
func (a *Assembler) worker() {
	defer a.wg.Done()

	open := make(map[fileKey]*openFile)
	completed := make(map[fileKey]struct{})    // tombstone set for finished files
	cancelledJobs := make(map[string]struct{}) // tombstone set for cancelled and closed jobs
	reqCount := 0

	reqsClosed := false

mainLoop:
	for {
		select {
		case req, ok := <-a.reqs:
			if !ok {
				// Channel was closed; this path is not taken in normal operation
				// (we never close reqs), but defend against it. Breaks to the
				// shared shutdown block below rather than returning here, so
				// this path cannot skip syncAndCloseAll and leave handles
				// open and files unsynced.
				reqsClosed = true
				break mainLoop
			}
			reqCount += a.dispatchRequest(req, open, completed, cancelledJobs)
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
			// WriteRequest has a nil ackCh, so it matches no
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
					reqCount += a.dispatchRequest(req, open, completed, cancelledJobs)
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
	// step. Every open file is fsynced before its handle closes, so a clean
	// shutdown leaves nothing buffered.
	a.syncAndCloseAll(open)
}

// dispatchRequest handles a single request from the channel. It processes the
// control messages — each a non-nil ackCh plus a FileIdx sentinel —
// skips articles for jobs the cancel or close-handles arm tombstoned, and delegates normal write
// requests to processRequest. Returns 1 if a normal request was processed (for
// reqCount tracking), 0 otherwise.
//
// The control messages, in the order the arms below test for them:
// fileIdxCancelJob (close and remove a job's files), fileIdxCloseHandles (close
// a job's handles, leaving the files), fileIdxForgetJob (drop a job's
// tombstones so a retry under the same ID can write again) and fileIdxQuiesce.
//
// This method is called from both the main select loop and the shutdown
// drain loop to ensure cancel messages are handled correctly in both paths.
func (a *Assembler) dispatchRequest(
	req WriteRequest,
	open map[fileKey]*openFile,
	completed map[fileKey]struct{},
	cancelledJobs map[string]struct{},
) int {
	// Control messages are told from articles by an unexported field being
	// set, not by the FileIdx sentinel alone.
	//
	// The sentinel is the encoding; it is not the proof. JobID and FileIdx are
	// both exported and both overwritten from the caller's ArticleRef, so on
	// the sentinel alone a caller outside this package could submit an article
	// with an empty JobID and a negative FileIdx and have the worker act on it
	// as a control message. ackCh is unexported, so no value built outside
	// internal/assembler can have it set, and the confusion is
	// unrepresentable rather than rejected.
	//
	// A zero WriteRequest is excluded by the same test, so it would reach
	// processRequest rather than any arm here. The worker does not dispatch
	// one: both of its receive loops break on a closed channel instead.
	if req.ackCh != nil && req.FileIdx == fileIdxCancelJob {
		// Control message: cancel a job. Close every open file for the job
		// encoded in MessageID, and unlink them or leave them on disk
		// according to req.disposition.
		//
		// The job-level tombstone is set under BOTH dispositions, and it has
		// to be: it gates article admission for the WHOLE job at the bottom of
		// this function, including files that were never opened, and
		// openTargetFile performs no queue-membership check before creating
		// and preallocating one. RemoveJob's pipeline.forgetJob closes that
		// hole too, but only after this returns, and not for requests already
		// queued behind this message.
		//
		// After this arm it is the job's only tombstone: dropFileTombstones
		// clears the per-file ones, and the one write to completed, in
		// finalizeFile, is reached only past dispatchRequest's cancelledJobs
		// check.
		// TestCancelJob_KeepFilesStillTombstonesTheWholeJob is the pin.
		cancelID := req.MessageID
		cancelledJobs[cancelID] = struct{}{}
		dropFileTombstones(completed, cancelID)
		for k, f := range open {
			if k.jobID != cancelID {
				continue
			}
			a.closeCancelledFile(f, req.disposition)
			// Dispatcher-level bookkeeping, and unconditional. What happens to
			// the BYTES is the disposition's business and lives in the helper;
			// which keys this worker still tracks is this loop's, and a kept
			// file has left the open map exactly as a deleted one has.
			delete(open, k)
		}
		// Unconditional: this branch is guarded on req.ackCh != nil.
		close(req.ackCh)
		return 0
	}
	if req.ackCh != nil && req.FileIdx == fileIdxCloseHandles {
		// Control message: close all open file handles for a job without deleting files.
		//
		// The job-level tombstone refuses every later article for the job,
		// including one for a file it never opened, which openTargetFile would
		// otherwise create under the post-processor with an fd nothing closes.
		// Dispatch stops at the hand-off (downloader Options.HandedOff), but an
		// article already in flight still arrives. ForgetJob clears it for a
		// retry. TestCloseJobHandles_TombstonesTheWholeJob is the pin. As in
		// the cancel arm, the job's per-file tombstones are dropped with it.
		targetID := req.MessageID
		cancelledJobs[targetID] = struct{}{}
		dropFileTombstones(completed, targetID)
		var closeErr error
		for k, f := range open {
			if k.jobID != targetID {
				continue
			}
			cerr := a.syncAndClose(f)
			if cerr != nil {
				// Recorded on the ack, not swallowed: a file whose close-time
				// sync or close failed has written bytes that may never have
				// reached the platter, and enqueuePostProc fails the post-processing run
				// on any fault this is, permanent or retryable. The file is
				// untrusted too: this fsync consumed the error, so no later
				// fsync at verification would report it.
				closeErr = errors.Join(closeErr, cerr)
				a.noteFileUntrusted(k.jobID, k.fileIdx)
			}
			delete(open, k)
		}
		if req.ackCh != nil {
			if closeErr != nil {
				req.ackCh <- closeErr
			}
			close(req.ackCh)
		}
		return 0
	}
	if req.ackCh != nil && req.FileIdx == fileIdxForgetJob {
		// Control message: drop a job's tombstones so a retry under the same
		// ID can write its files again. See Assembler.ForgetJob.
		//
		// The open map is untouched on purpose. This forgets that files were
		// FINISHED; it asserts nothing about one currently being written, and
		// closing a live handle here would leave its writer's unreported articles stranded.
		forgetID := req.MessageID
		dropFileTombstones(completed, forgetID)
		delete(cancelledJobs, forgetID)
		if req.ackCh != nil {
			close(req.ackCh)
		}
		return 0
	}
	if req.ackCh != nil && req.FileIdx == fileIdxQuiesce {
		// Control message: everything queued ahead of this has been written
		// and its callbacks have run. See Assembler.Quiesce.
		close(req.ackCh)
		return 0
	}
	// An article request the worker received ends here: the control arms
	// above have returned, and the worker's two receive loops are this
	// function's callers (`git grep -n 'a\.dispatchRequest(' --
	// 'internal/assembler/*.go' ':!*_test.go'` returns 2 lines).
	if req.Release != nil {
		defer req.Release()
	}
	// Skip articles for cancelled and closed jobs.
	if _, cancelled := cancelledJobs[req.JobID]; cancelled {
		if req.Data != nil {
			a.releaseBuffer(req.Data)
		}
		return 0
	}
	a.processRequest(req, open, completed)
	return 1
}

// dropFileTombstones deletes jobID's entries from completed.
//
// The cancel and close-handles arms call it once they have set the job-level
// tombstone, which dispatchRequest checks before processRequest reads
// completed: from then on the job's per-file entries are never consulted, and
// keeping them would hold one per finished file until a retry's ForgetJob, the
// only other caller.
func dropFileTombstones(completed map[fileKey]struct{}, jobID string) {
	maps.DeleteFunc(completed, func(k fileKey, _ struct{}) bool { return k.jobID == jobID })
}

// syncAndClose fsyncs a file and closes it, and reports whether either
// failed. The close-handles arm and syncAndCloseAll
// untrust the file on an error (noteFileUntrusted): a failed fsync means the
// bytes its articles were reported written with cannot be trusted, and the
// untrust is what returns those articles to Outstanding. This returns nothing
// itself: the articles were written, so they are Done, not Emitted, and
// OnArticlesUnwritten would have nothing to clear.
//
// # Why the fault is REPORTED and not ROUTED
//
// On the CloseJobHandles path the job is already admitted to post-processing
// (app's postProcAdmissions). Fail cannot hand it over again — enqueuePostProc
// refuses an admitted job, at most attaching the reason to the admitted run,
// which is what enqueuePostProc does itself with any fault this returns — and
// Stall would pause a job whose files post-processing is using.
//
// A permanent fault is preferred over the first one when they differ, because
// only the permanent one preserves R20. ENOSPC on the fsync followed by EROFS
// on the close — an ext4 mounted errors=remount-ro, the Debian default — is
// the case that makes the difference: reporting the ENOSPC alone describes the
// condition as one that waiting can clear, when it cannot.
func (a *Assembler) syncAndClose(f *openFile) error {
	var first, permanent error
	note := func(op string, err error) {
		if err == nil {
			return
		}
		a.log.Warn(op, "path", f.info.Path(), "error", err)
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

	note("sync file before close", f.w.sync())
	// A failing close is a storage condition too, and on network-backed mounts
	// it is frequently the first report of writes that never landed — the
	// close is where a deferred error surfaces.
	note("close file", f.w.close())

	if permanent != nil {
		return permanent
	}
	return first
}

// closeCancelledFile releases one open file of a cancelled job, and disposes
// of its bytes according to disposition. Runs on the worker goroutine, which
// owns every handle (X1).
//
// It is the cancel counterpart to syncAndClose, and deliberately not a call
// to it. syncAndClose calls sync, which CloseJobHandles needs because par2 and
// unrar are about to read the file. Nothing reads a removed job's files, and
// an fsync here would stall ingest for every other job on the one worker
// goroutine in exchange for nothing.
//
// Nothing is routed: no OnFileUntrusted, no OnArticlesUnwritten, no
// OnArticleRejected. The job has already left the
// queue, so returning its articles to Outstanding would re-dispatch work for a
// job that is going away, under either disposition (closeleak_test.go).
//
// "Already left the queue" is exact rather than hedging: RemoveJob is the only
// production caller of this CancelJob — `git grep -n 'assembler\.CancelJob(' -- '*.go' ':!*_test.go'`
// returns 1 line, in Application.RemoveJob — and since #376 it removes the job
// from the queue BEFORE calling in, returning early if that fails.
//
// The caller keeps the bookkeeping: the open-map delete is unconditional, and
// is not this function's business.
func (a *Assembler) closeCancelledFile(f *openFile, disposition FileDisposition) {
	// The close error is best-effort only under DeleteFiles, where the file is
	// unlinked below and nothing it failed to flush could ever be read. Under
	// KeepFiles the file survives, so the same error means a kept file may be
	// missing bytes, and it is logged rather than discarded.
	cerr := f.w.close()
	if cerr != nil && disposition == KeepFiles {
		a.log.Warn("failed to close a cancelled job's file that is being kept; "+
			"unsynced bytes may not have reached the platter",
			"path", f.info.Path(), "error", cerr)
	}
	if disposition == DeleteFiles {
		if err := fsutil.Remove(f.info.Path()); err != nil && !os.IsNotExist(err) {
			a.log.Warn("failed to remove cancelled file",
				"path", f.info.Path(), "error", err)
		}
	}
}

// syncAndCloseAll fsyncs and closes every remaining open file. Called on
// worker exit. Completion callbacks do NOT fire for partial files — writing
// N-of-M parts is not a completion event.
func (a *Assembler) syncAndCloseAll(open map[fileKey]*openFile) {
	for k, f := range open {
		// The fault itself has nowhere to go: this runs on worker exit, so
		// there is no caller left to answer, and routing it to OnWriteFault is
		// unsafe — Application.Shutdown joins this goroutine through
		// waitBounded, which ABANDONS its step after the budget and returns
		// while the worker runs on, and a fault handler on app.wg.Go racing
		// app.wg.Wait is a WaitGroup misuse panic.
		//
		// The file is still untrusted, synchronously on this goroutine: this
		// fsync consumed the error, so the next start's verification fsync
		// would not see it.
		if err := a.syncAndClose(f); err != nil {
			a.noteFileUntrusted(k.jobID, k.fileIdx)
		}
	}
}

// processRequest performs the WriteAt for a single WriteRequest. It resolves
// the target file on first encounter, caches the handle, and hands the file to
// finalizeFile once all TotalParts have been accounted for. Each article is
// written individually, through fileWriter.accept.
func (a *Assembler) processRequest(req WriteRequest, open map[fileKey]*openFile, completed map[fileKey]struct{}) {
	key := fileKey{jobID: req.JobID, fileIdx: req.FileIdx}

	if _, done := completed[key]; done {
		a.handleLateDuplicate(req)
		return
	}

	f, ok := open[key]
	if !ok {
		var err error
		f, err = a.openTargetFile(key, req, open)
		if errors.Is(err, ErrJobClosed) {
			// The resolver says the job no longer accepts writes. Dropped as
			// an article for a cancelled job is: nothing is opened, nothing
			// is reported, and the buffer goes back. The article stays
			// Emitted, which is what a late article for a closed job was
			// before too.
			if req.Data != nil {
				a.releaseBuffer(req.Data)
			}
			return
		}
		if err != nil {
			// Routed through OnWriteFault, because nothing else can reach
			// it: the file is never inserted into open, so no later fsync or
			// close can return it. Dropped, a persistent EACCES or EROFS on the
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
			// Reported alongside the fault: there is no fileWriter to have
			// rolled it back, because the file was never opened. The article
			// is nevertheless un-written and still Emitted, which is not
			// Outstanding.
			a.noteArticlesUnwritten(req.JobID, req.FileIdx, []int32{req.ArtIdx})
			a.noteWriteFault("", req, err)
			if req.Data != nil {
				a.releaseBuffer(req.Data)
			}
			return
		}
	}

	// The part count is the writer's: fileWriter.admit and fileWriter.resolve
	// move it, in the same breath as the seen-set record it is derived from,
	// and the handlers below report nothing back. A count applied here, after
	// the write, could be rolled back by that write's own failure: the
	// give-back is decided from seenDone membership, so the two were set at
	// different moments and the rollback reversed a count that had not been
	// applied yet. Every transient accept-time write fault undercounted the
	// file by one, permanently, and partsWritten >= TotalParts became
	// unreachable.
	//
	// Whether this request moved the count is read from the writer rather
	// than relayed by the handler. A request that moved nothing — a duplicate,
	// a retry of an article already counted, a rolled-back write — is not a
	// processed part, so it is neither logged as one nor checked against
	// TotalParts.
	before := f.w.parts()
	if req.FatalErr != nil {
		a.handleFatalArticle(f, req)
	} else {
		a.handleSuccessArticle(f, req)
	}
	// Read once, so the count the log reports is provably the count the
	// comparison below decided on.
	parts := f.w.parts()
	if parts == before {
		return
	}
	a.log.Debug("processed part",
		"job", req.JobID, "fileidx", req.FileIdx,
		"part", parts, "total", f.info.TotalParts,
		"offset", req.Offset, "bytes", len(req.Data), "failed", req.FatalErr != nil)
	if f.info.TotalParts > 0 && parts >= f.info.TotalParts {
		a.finalizeFile(f, key, req, open, completed)
	}
}

// handleLateDuplicate handles articles arriving for a file that is already
// tombstoned. The file's writer is gone by then: the one tombstone write, in
// finalizeFile, follows its `delete(open, key)` (`git grep -n -E
// 'completed\[[a-z]+\] = ' -- 'internal/assembler/*.go' ':!*_test.go'` returns
// 1 line), and openTargetFile, the one insert into open, is
// reached only past processRequest's tombstone check.
//
// The article is not written: the file was finished, trimmed and reported
// complete, and the job's record says what it holds.
//
// # The article that is owed something
//
// An article can reach here with no record of it in the job: a RolledBack
// gives the part back for every article whose write failed, the downloader
// re-dispatches it, and the copy that comes back arrives after its file was
// tombstoned. Dropping it would strand it — not written, not failed, and
// Emitted again, which ForEachUnfinishedArticle skips — so the job would never
// finish.
//
// It is reported as rejected, which resolves it as permanently failed: the
// file is finished and this package cannot write the article now or later.
// Failing it charges its bytes against the job's par2 recovery budget, which
// is exactly the decision par2 exists to make.
//
// With the writer gone nothing here knows which articles it accounted for, so
// every late article is reported: Job.MarkArticleFailed is a no-op for an
// article already Done (setFailedBits), so an accepted article costs a log
// line and nothing else.
func (a *Assembler) handleLateDuplicate(req WriteRequest) {
	a.log.Debug("ignoring late article for completed file",
		"job", req.JobID, "fileidx", req.FileIdx, "msgid", req.MessageID)
	if req.Data != nil {
		a.releaseBuffer(req.Data)
	}
	if a.opts.OnArticleRejected != nil {
		a.opts.OnArticleRejected(req.JobID, req.FileIdx, req.ArtIdx,
			"redelivered after its file was completed", WholeArticle)
	}
}

// openTargetFile resolves file information and creates the target file on disk
// for a new fileKey.
//
// The name comes from the NZB, so it is untrusted: the file is opened through
// openInDir, which opens that one file in FileInfo.Dir and never another. A
// name that leads out of the job directory, or any link (symbolic or hard) in
// the file's place, fails the open, and that failure takes the same path as
// any other open error.
//
// Every failure returns a classified *storagefault.Fault rather than logging
// and discarding the article (#357). The distinction matters because of A2: a
// dropped article is resolved neither way, so no later run re-dispatches it and
// the job stalls at 99% forever.
//
// processRequest, the caller, acts on a returned fault: it returns the article
// to Outstanding through OnArticlesUnwritten and hands the fault to
// OnWriteFault, which stalls or fails the job by the fault's class.
//
// A failing FileInfo resolver is classified against the path it could not
// produce, which is empty — the honest answer, since the resolver failed before
// there was a path. It is retryable by default under R18's "anything
// unrecognised is retryable" rule, which is the correct direction: a resolver
// failure is usually a job whose manifest is momentarily non-resident.
//
// It never releases req.Data, on any return: it reads req only for the
// article's identity, and its caller owns the buffer throughout. It used to
// release on each failure return while processRequest released on the error it
// got back, putting one backing array in decoder's pool twice (#574).
func (a *Assembler) openTargetFile(key fileKey, req WriteRequest, open map[fileKey]*openFile) (*openFile, error) {
	info, err := a.opts.FileInfo(req.JobID, req.FileIdx)
	if err != nil {
		if errors.Is(err, ErrJobClosed) {
			// Not a storage fault: the job has left the queue or been handed
			// to post-processing, and processRequest drops the article.
			return nil, err
		}
		return nil, storagefault.Classify("resolve", "", err)
	}

	path := info.Path()
	if err := os.MkdirAll(info.Dir, 0o750); err != nil {
		return nil, storagefault.Classify("mkdir", path, err)
	}
	fh, err := openInDir(info.Dir, info.Name)
	if err != nil {
		return nil, storagefault.Classify("open", path, err)
	}
	if info.ExpectedSize > 0 {
		telemetry.PreallocCalls.Add(1)
		if err := preallocateFile(fh, info.ExpectedSize); err != nil {
			a.log.Warn("file pre-allocation failed, continuing without",
				"path", path,
				"size", info.ExpectedSize,
				"error", err,
			)
		}
	} else {
		a.log.Debug("zero-length expected size for target file",
			"path", path,
		)
	}
	// No seeded high-water mark, and no seeded write cursor. Both used to
	// carry forward how far EARLIER PROCESSES — previous starts of the daemon
	// — had written this file, so the completion truncate would not cut away
	// the bytes those processes put there (#342). The truncate no longer
	// derives its bound from anything this process measured: finish bounds it by
	// the file's ownedRanges (maxEnd, finish.go), which seedOwned fills from the
	// record below, so there is nothing left for a seed to protect.
	f := &openFile{
		w:    newFileWriter(fh, path, key),
		info: info,
	}
	if syncFn := a.opts.SyncFile; syncFn != nil {
		f.w.syncFile = func() error { return syncFn(fh) }
	}
	a.seedOwned(f.w, info.Owned)
	open[key] = f
	return f, nil
}

// openInDir opens name inside dir for writing, creating it if absent, through
// fsutil.OpenNoFollow on an os.Root on dir. A job file is always a regular
// file with one link; any link in name's place, or a name that is not one
// path component, is an open error. Writing through a link to a sibling
// would write one file's bytes into another. The root is closed before
// returning; the file's descriptor does not depend on it.
func openInDir(dir, name string) (*os.File, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }() // a directory handle; nothing to lose on close
	return fsutil.OpenNoFollow(root, name, os.O_WRONLY|os.O_CREATE, 0o644)
}

// seedOwned seeds w's range set from ranges verified before this process;
// openTargetFile passes it FileInfo.Owned.
//
// A zero-length range is a valid row (a zero-length article is Done and claims
// no range, durability-contract.md "A zero-length article claims nothing"): it
// passes this check silently, ownedRanges.seed discards it, and it is not
// refetched. The ranges come from disk, so each is checked first: Off >= 0,
// Len >= 0 and Off+Len not overflowing. A malformed range is dropped
// with a warning. That is safe under Standing Design Rule 3: an unseeded range
// only means its articles are fetched again, which costs their own bytes.
//
// seed fails only on a non-empty set, and this runs on a fresh writer, so its
// error is a programming error and is logged at Error level rather than
// returned: the file still opens, unseeded, and the cost is the same refetch.
func (a *Assembler) seedOwned(w *fileWriter, owned []Range) {
	if len(owned) == 0 {
		return
	}
	valid := make([]Range, 0, len(owned))
	for _, r := range owned {
		if r.Off < 0 || r.Len < 0 || r.Off > math.MaxInt64-r.Len {
			a.log.Warn("dropping invalid owned range",
				"job", w.key.jobID, "fileidx", w.key.fileIdx, "off", r.Off, "len", r.Len)
			continue
		}
		valid = append(valid, r)
	}
	if err := w.owned.seed(valid); err != nil {
		a.log.Error("seeding owned ranges failed",
			"job", w.key.jobID, "fileidx", w.key.fileIdx, "error", err)
	}
}

// handleFatalArticle counts a permanently failed article toward the file's
// part total without writing anything.
//
// It takes ownership of req.Data and releases it before returning, on every
// path, matching handleSuccessArticle and fileWriter.accept's contract: the
// caller never has to reason about who frees it. Safe here because this
// function writes nothing to disk on any path, so it never needs the bytes to
// survive the call — unlike accept, which must hold them until they are
// written before it can let go.
//
// It records no ack. A permanent failure is the queue's to record via
// Job.MarkArticleFailed (R10), and this package no longer has an ack path in
// either direction. The dedup that keeps partsWritten from overshooting
// TotalParts is purely local bookkeeping, and it lives on the writer with the
// seen-sets it reads and the counter it moves — resolve's FetchFailed arm.
func (a *Assembler) handleFatalArticle(f *openFile, req WriteRequest) {
	a.log.Debug("counting failed article toward completion (skipping disk write)",
		"job", req.JobID, "fileidx", req.FileIdx, "path", f.info.Path(), "error", req.FatalErr)
	f.w.resolve(req.ArtIdx, dispositionFetchFailed)
	if req.Data != nil {
		a.releaseBuffer(req.Data)
	}
}

// handleSuccessArticle hands one article's bytes to the file's writer and
// resolves what came of it.
//
// The shape is the point: one admission, one outcome, one resolve, one route.
// acceptArticle decides the disposition and returns it as a value, so every
// return path there has to name one; fileWriter.resolve applies the
// accounting that disposition owes, so this function never chooses it; and
// routeOutcome reports it to the callback that owns that disposition. The
// caller reads the part count afterwards (processRequest), so nothing here
// has to say whether the article counted.
func (a *Assembler) handleSuccessArticle(f *openFile, req WriteRequest) {
	w := f.w
	id := articleID{artIdx: req.ArtIdx}
	// Admitted before the write is attempted, not after, so the write's own
	// failure can roll the admission back — and admitted in the same breath
	// as the count, which is what admit exists to make inseparable. Whether
	// the admission charges a part is the writer's to decide: a retry of an
	// article already counted as failed is admitted so its bytes are written,
	// without counting it twice.
	if !w.admit(req.ArtIdx) {
		// A duplicate of an article already accepted. Its first copy is
		// already written; this copy's bytes are redundant, and re-writing
		// them would be a second WriteAt for the same range.
		if req.Data != nil {
			a.releaseBuffer(req.Data)
		}
		return
	}
	o := a.acceptArticle(f, id, req)
	w.resolve(req.ArtIdx, o.disposition)
	a.routeOutcome(f, req, o)
}

// outcome is what acceptArticle decided for one admitted article: its
// disposition, and what the route for that disposition reports. Each field
// after the disposition belongs to one arm and is zero for the others; every
// literal that builds one sets the disposition and its fields in one
// statement, so a half-built outcome is not a state a caller can reach.
type outcome struct {
	disposition disposition

	// n is the length of the bytes handed to WriteAt, for OnArticleWritten
	// (dispositionWritten).
	n int64

	// reason and uncovered are OnArticleRejected's (dispositionRejected): why
	// the article was refused, and how many of its bytes no other article's
	// range covers, or WholeArticle when none of it is on disk.
	reason    string
	uncovered int64

	// fault is the *storagefault.Fault the write raised
	// (dispositionRolledBack).
	fault error
}

// acceptArticle range-checks the write and hands it to the file's writer,
// returning the article's disposition with what its route needs.
//
// It applies no accounting and reports nothing: the caller resolves the
// disposition with the writer and routes the outcome. Returning the
// disposition as a value is what makes a new exit path loud rather than
// silent — a path that returns here has to say what it decided, and an
// outcome that says nothing reaches resolve as the zero disposition, which
// resolve refuses.
//
// A write refused or faulted here leaves nothing behind for a later fsync to
// fail on, so the outcome is the only way the fact gets out. Nor is a faulted
// article Outstanding: its Emitted bit is still set, and
// ForEachUnfinishedArticle skips it, so it is never re-dispatched.
//
// Returning the fault is what makes A1 true rather than merely asserted: the
// caller rolls the part back, returns the article through
// OnArticlesUnwritten, hands the fault to OnWriteFault, and the job stalls on a
// storage condition without any article being recorded as damaged.
func (a *Assembler) acceptArticle(f *openFile, id articleID, req WriteRequest) outcome {
	if reason, ok := a.offsetOutOfRange(f, req); ok {
		if req.Data != nil {
			a.releaseBuffer(req.Data)
		}
		return outcome{disposition: dispositionRejected, reason: reason, uncovered: WholeArticle}
	}
	// A byte range another article has already written is owned, and the
	// ARRIVING article is the one refused rather than the incumbent. The
	// incumbent's row may already be buffered by the recorder
	// (OnArticleWritten) over exactly its range with its CRC; overwriting those
	// bytes leaves a record describing bytes the file no longer holds. An article whose write faulted claimed
	// nothing (see fileWriter.owned), so it never makes a later arrival lose.
	//
	// Checked here rather than inside accept so the refusal travels the same
	// route as the out-of-range one above — accept's contract is that its error
	// always reports STORAGE failing, and this reports the article.
	//
	// The refusal carries how many of the article's bytes no other article's
	// range covers (#432): the covered ones are on disk, so only the rest is
	// owed to par2.
	rng := Range{Off: req.Offset, Len: int64(len(req.Data))}
	if _, settled := f.w.owned.ownerOf(rng); settled {
		uncovered := rng.Len - f.w.owned.covered(rng)
		if req.Data != nil {
			a.releaseBuffer(req.Data)
		}
		return outcome{
			disposition: dispositionRejected,
			reason:      "claims a byte range already written by another article",
			uncovered:   uncovered,
		}
	}
	n := int64(len(req.Data)) // accept returns the buffer to the pool
	if err := f.w.accept(id, req.Offset, req.Data); err != nil {
		return outcome{disposition: dispositionRolledBack, fault: err}
	}
	return outcome{disposition: dispositionWritten, n: n}
}

// routeOutcome reports one resolved article to the callback its disposition
// owns, which is the A1 split in one place: a rejection says nothing about
// storage and goes to OnArticleRejected; a storage fault says nothing about
// the article and goes to OnWriteFault, with the rolled-back article handed
// to OnArticlesUnwritten because its Emitted bit is still set and nothing
// else clears it, as the openTargetFile failure does.
//
// Only the three dispositions acceptArticle produces have an arm. A
// FetchFailed never comes through here — handleFatalArticle resolves it and
// the queue already holds the failure — and the zero disposition is refused
// by resolve before this runs, so the default arm is unreachable by
// construction and panics rather than dropping a report.
func (a *Assembler) routeOutcome(f *openFile, req WriteRequest, o outcome) {
	switch o.disposition {
	case dispositionWritten:
		// The cached statfs reading does not see these bytes until it is
		// refreshed (#987).
		a.diskProbe.NoteWritten(o.n)
		if a.opts.OnArticleWritten != nil {
			a.opts.OnArticleWritten(req.JobID, req.FileIdx, req.ArtIdx, req.Offset, o.n, req.CRC32)
		}
	case dispositionRejected:
		a.log.Warn("article rejected; it will be recorded as permanently failed",
			"job", req.JobID, "fileidx", req.FileIdx, "artidx", req.ArtIdx,
			"msgid", req.MessageID, "path", f.info.Path(), "reason", o.reason)
		if a.opts.OnArticleRejected != nil {
			a.opts.OnArticleRejected(req.JobID, req.FileIdx, req.ArtIdx, o.reason, o.uncovered)
		}
	case dispositionRolledBack:
		a.noteArticlesUnwritten(req.JobID, req.FileIdx, []int32{req.ArtIdx})
		a.noteWriteFault(f.info.Path(), req, o.fault)
	default:
		panic(fmt.Sprintf("assembler: article %d routed with %s, which acceptArticle does not produce", req.ArtIdx, o.disposition))
	}
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

// finalizeFile finishes a file whose parts have all arrived and reports it.
//
// The finish runs here, on the worker and on the file's writing handle, before
// anything is reported: fsync, truncate to the end of the last owned byte,
// fsync again (fileWriter.finish), then close. Only a file all of that
// succeeded for is tombstoned and reported through OnFileComplete, so a caller
// that records the file complete records it after its fsync.
//
// Any error is a completion fault. The handle is closed and dropped from open,
// but the file is NOT tombstoned: a tombstone would send every refetched
// article to handleLateDuplicate, which writes nothing once the handle is
// gone, so the refetch would never converge. The first refetched article
// re-opens the file through the FileInfo resolver and a fresh writer instead.
// OnFileUntrusted tells the caller the file's written articles are no longer
// vouched for, and OnWriteFault routes the classified fault (Stall or Fail).
func (a *Assembler) finalizeFile(f *openFile, key fileKey, req WriteRequest,
	open map[fileKey]*openFile, completed map[fileKey]struct{}) {
	err := f.w.finish()
	if cerr := f.w.close(); err == nil {
		err = cerr
	}
	delete(open, key)
	if err != nil {
		a.log.Error("completed file could not be finished; its articles are refetched",
			"job", req.JobID, "fileidx", req.FileIdx, "path", f.info.Path(), "error", err)
		a.noteFileUntrusted(req.JobID, req.FileIdx)
		fault, ok := errors.AsType[*storagefault.Fault](err)
		if !ok {
			fault = storagefault.Classify("finish", f.info.Path(), err)
		}
		if a.opts.OnWriteFault != nil {
			a.opts.OnWriteFault(req.JobID, req.FileIdx, fault)
		}
		return
	}
	completed[key] = struct{}{} // tombstone: reject late duplicates
	telemetry.FilesCompleted.Add(1)
	a.log.Info("file complete", "job", req.JobID, "fileidx", req.FileIdx, "path", f.info.Path())
	if a.opts.OnFileComplete != nil {
		a.opts.OnFileComplete(req.JobID, req.FileIdx)
	}
}

// noteFileUntrusted hands one untrusted file to its owner.
func (a *Assembler) noteFileUntrusted(jobID string, fileIdx int) {
	if a.opts.OnFileUntrusted != nil {
		a.opts.OnFileUntrusted(jobID, fileIdx)
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
		dir := f.info.Dir
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
		timeout := a.opts.DiskCheckTimeout
		if timeout <= 0 {
			timeout = diskCheckTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
// FileInfo.ExpectedSize), so a legitimate decoded write never exceeds it. NZB
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
// produce a file of arbitrary apparent size. The completion trim does not
// commit that size — finish bounds it by ownedRanges.maxEnd — but the sparse file
// itself is still the attack, so the offset is rejected before the write.
//
// It returns the reason rather than a bare bool because the rejection has to
// reach the queue: the article is refused here, and nothing else in the system
// will refuse it again, so a caller that only knew "not written" could neither
// record it failed nor say why.
func (a *Assembler) offsetOutOfRange(f *openFile, req WriteRequest) (string, bool) {
	reject := func(reason string) (string, bool) {
		a.log.Warn("rejecting out-of-range article write offset",
			"path", f.info.Path(),
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
