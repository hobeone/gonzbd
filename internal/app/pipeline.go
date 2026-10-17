package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/telemetry"
)

// isRetryableDownloaderError returns true for transient errors where the
// dispatcher will retry the article (on the same or another server):
//   - 430 / ErrNoArticle: not on this server, try fallback
//   - 480 / ErrAuthRequired: connection needs re-auth (dispatcher cycles conn)
//   - 502/503 / ErrServerUnavailable: server temporarily refusing service
//   - 4xx / ErrTransient: generic NNTP transient error
//   - ErrClosed: connection was torn down (server disconnect, timeout)
//   - io.ErrUnexpectedEOF: premature server disconnect mid-transfer
//   - ErrCRCMismatch: article data CRC doesn't match — try another server
//   - context.Canceled / context.DeadlineExceeded: a shutdown or a deadline cut
//     this attempt short, which says nothing about the article
//   - Dial errors / connection resets / I/O timeouts
//
// Terminal errors (other decode failures, ErrAuthRejected, etc.) return false.
// The caller records those with Job.MarkArticleFailed and then still hands
// the article to the assembler with FatalErr set, so the file's part count
// reaches its total and the file can complete with a hole in it. The failure
// accounting is the pipeline's; the assembler only counts parts.
func isRetryableDownloaderError(err error) bool {
	// Sentinel-based checks (preferred — no string fragility).
	sentinels := [...]error{
		nntp.ErrNoArticle,
		nntp.ErrServerUnavailable,
		nntp.ErrAuthRequired,
		nntp.ErrTransient,
		nntp.ErrClosed,
		io.ErrUnexpectedEOF,
		context.Canceled,
		context.DeadlineExceeded,
		decoder.ErrCRCMismatch,
	}
	for _, s := range sentinels {
		if errors.Is(err, s) {
			return true
		}
	}
	// Network-level errors: op errors (dial, read, write) and timeouts.
	// Using the type hierarchy is more robust than string-matching against
	// error messages that can change across Go releases or OS versions.
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	if timeoutErr, ok := errors.AsType[interface {
		error
		Timeout() bool
	}](err); ok && timeoutErr.Timeout() {
		return true
	}
	return false
}

// fileKey uniquely identifies a file within a job.
type fileKey struct {
	jobID   string
	fileIdx int
}

// refFor is the sole place a downloaded article's identity becomes an
// assembler.ArticleRef.
//
// Both write paths need the same four fields off the same result, and writing
// them out twice would leave the derivation with two authors a hundred lines
// apart: adding a fifth identity field means finding both, and editing one
// means they disagree with nothing able to notice, since they currently agree
// by construction. That is the shape ArticleRef exists to remove from the
// assembler's caller contract, so it should not be reintroduced here.
func refFor(res *downloader.ArticleResult) assembler.ArticleRef {
	return assembler.ArticleRef{
		JobID:     res.JobID(),
		FileIdx:   res.FileIdx,
		ArtIdx:    res.ArtIdx,
		MessageID: res.MessageID,
	}
}

// pipeline plumbs Downloader.Completions() → assembler; the downloader has
// already decoded each body (internal/downloader's dispatch.go calls the
// decoder). The reader goroutine (run) fans out ArticleResults to a pool of worker
// goroutines that call handleResult concurrently. The fileInfo map is
// populated by workers and read by the assembler worker via resolveFileInfo,
// so access is protected by mu.
type pipeline struct {
	log         *slog.Logger
	dispatcher  *dispatch.Dispatcher
	assembler   *assembler.Assembler
	completions <-chan *downloader.ArticleResult
	downloadDir string
	sanitize    fsutil.SanitizeOptions
	numWorkers  int // concurrent handleResult workers; 0 defaults to GOMAXPROCS

	// onJobHopeless is called when a job is detected as beyond repair
	// (either by byte-level health or early-abort heuristic).
	onJobHopeless func(jobID string)

	// onHeartbeat is called when an article result is processed.
	onHeartbeat func()

	// markDirty records one file's state for the recorder's next flush
	// (Application.markFileDirty).
	markDirty func(j *job.Job, fileIdx int)

	// nameReadHook, when non-nil, runs once per registerFile attempt, right
	// after it reads the job's name and before it records the file's filename.
	nameReadHook func(j *job.Job)

	// countedHook, when non-nil, runs once per registerFile attempt, right
	// after it counts the file's unfinished articles and before it records
	// the file's filename.
	countedHook func(j *job.Job)

	// ctx is the context passed to run(); stored so setCompletions can
	// avoid blocking forever if run() has already exited.
	ctx context.Context

	// updateCh receives a swap request with a done channel for ack.
	updateCh chan completionSwap

	// inFlight counts results the run loop has handed to a worker but that
	// no worker has finished processing. It is what lets a swap wait for
	// the WRITES to land rather than only for the channel to empty; see
	// setCompletions.
	//
	// run() is the sole Adder and is also the goroutine that waits, so Add
	// never races Wait. The zero value is usable, which matters because
	// pipeline is built as a struct literal at a dozen sites.
	inFlight sync.WaitGroup

	mu       sync.RWMutex
	fileInfo map[fileKey]assembler.FileInfo
}

// completionSwap bundles a new completions channel with a done channel.
// The run loop closes done after fully switching, letting setCompletions
// block until the old channel is drained AND the results it carried have
// been written.
type completionSwap struct {
	ch   <-chan *downloader.ArticleResult
	done chan struct{}
}

// run is the pipeline's reader goroutine. It reads from p.completions
// and fans out to a pool of worker goroutines via a work channel.
// Returns when ctx is cancelled.
func (p *pipeline) run(ctx context.Context) {

	nw := p.numWorkers
	if nw <= 0 {
		nw = max(runtime.GOMAXPROCS(0), 2)
	}

	work := make(chan *downloader.ArticleResult, nw*2)
	var wg sync.WaitGroup
	for range nw {
		wg.Go(func() {
			for res := range work {
				p.handleResult(ctx, res)
				p.inFlight.Done()
			}
		})
	}
	defer func() {
		close(work)
		wg.Wait()
	}()

	p.log.Info("pipeline workers started", "workers", nw)

	for {
		select {
		case <-ctx.Done():
			return
		case swap := <-p.updateCh:
			// Drain any remaining results from the old channel before
			// switching. This prevents lost ArticleResults when the
			// old downloader's completions channel still has buffered
			// items after Stop().
			if p.completions != nil {
				for {
					select {
					case res, ok := <-p.completions:
						if !ok {
							goto drained
						}
						p.inFlight.Add(1)
						// work holds nw*2 and completions holds up to
						// 256, so a stalled worker can fill it and leave
						// this send blocked. Without the ctx arm that
						// blocks the swap — and therefore a reload or a
						// shutdown — behind storage that may never
						// answer. Done() before returning, or the count
						// is left claiming an item no worker has.
						select {
						case work <- res:
						case <-ctx.Done():
							p.inFlight.Done()
							res.Release()
							close(swap.done)
							return
						}
					default:
						goto drained
					}
				}
			}
		drained:
			// Emptying the channel is not the same as finishing the
			// work: the loop above only moved results into `work`, and
			// the pwrite happens later on a worker. Wait for those
			// writes, so a caller that swaps to nil gets a quiescence
			// point rather than a hand-off point. #390 is what the
			// difference costs — a reload that acked "everything
			// written" while a bufferful was still queued to be
			// written, then cleared the Emitted bits over the top.
			if !p.awaitInFlight(ctx) {
				// ctx ended mid-wait. Acknowledge the swap so the
				// caller is not stranded, but do NOT resume the loop:
				// a later Add racing the abandoned Wait is the one
				// WaitGroup misuse this design has to avoid.
				close(swap.done)
				return
			}
			p.completions = swap.ch
			close(swap.done)
		case res, ok := <-p.completions:
			if !ok {
				// Downloader stopped; wait for a new channel or cancellation.
				// Set completions to nil so we don't busy-spin on a closed channel.
				p.log.Info("Downloader stopped, waiting for new channel")
				p.completions = nil
				continue
			}
			p.inFlight.Add(1)
			// Same blocking send as the drain loop above, and the same
			// reason for the ctx arm: selecting ctx.Done() at the top of
			// this loop achieves nothing if the send it chose instead can
			// park forever on a stalled worker.
			select {
			case work <- res:
			case <-ctx.Done():
				p.inFlight.Done()
				res.Release()
				return
			}
		}
	}
}

// awaitInFlight blocks until every result handed to a worker has been
// processed, or ctx ends. Reports whether the wait completed.
//
// WaitGroup.Wait is not cancellable, so it runs on its own goroutine and
// the select provides the cancellation. That goroutine outlives a false
// return, but only until the writes it is waiting on finish — bounded by
// the same I/O the caller was already waiting for, not leaked.
//
// Only run() may call this: it is the sole Adder, and a concurrent Add
// would be a WaitGroup misuse rather than merely a lost wakeup.
func (p *pipeline) awaitInFlight(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		p.inFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// setCompletions swaps the source of ArticleResults. Blocks until the
// pipeline's run loop has drained any remaining results from the old
// channel, WRITTEN them, and acknowledged the swap.
//
// The write half is load-bearing and was once only claimed: draining the
// channel moves results onto a buffered work channel, so a caller that
// took "drained" to mean "written" was reading a comment, not a
// guarantee. setCompletions(nil) is a quiescence point for the old
// downloader's output, and ReloadDownloader's Emitted clear depends on it
// being one.
func (p *pipeline) setCompletions(ch <-chan *downloader.ArticleResult) {
	done := make(chan struct{})
	select {
	case p.updateCh <- completionSwap{ch: ch, done: done}:
	case <-p.ctx.Done():
		return
	}
	select {
	case <-done:
	case <-p.ctx.Done():
	}
}

// setSanitize sets the options newJobFileLocation names a file with from
// here on. A file already named keeps its recorded name (jobFileLocation).
func (p *pipeline) setSanitize(o fsutil.SanitizeOptions) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sanitize = o
}

// sanitizeOptions returns the naming replacement options in effect.
func (p *pipeline) sanitizeOptions() fsutil.SanitizeOptions {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sanitize
}

// handleResult processes one downloader output, whose body the downloader has
// already decoded: it drops a result for a job instance no longer registered,
// and otherwise hands it to handleFailureResult or handleSuccessResult, which
// register the file on first encounter and hand the article to the assembler.
//
// A retryable failure, or an article that cannot be registered, or that the
// assembler refuses for a reason other than context.Canceled, goes back to
// the dispatch pool (clearEmitted); a permanent failure is
// recorded on the job (Job.MarkArticleFailed) and can trigger the early-abort
// hand-off (onJobHopeless).
func (p *pipeline) handleResult(ctx context.Context, res *downloader.ArticleResult) {
	telemetry.ArticlesReceived.Add(1)
	if p.onHeartbeat != nil {
		p.onHeartbeat()
	}

	if !p.isCurrent(res) {
		p.log.Debug("dropping a result fetched for a job instance that is no longer registered",
			"job", res.JobID(), "msgid", res.MessageID, "err", res.Err)
		res.Release()
		return
	}

	if res.Err != nil {
		p.handleFailureResult(ctx, res)
	} else {
		p.handleSuccessResult(ctx, res)
	}
}

// isCurrent reports whether res was fetched for the job instance the
// dispatcher now holds under its ID. A fetch can outlive its instance's
// failure, finalization and a retry, which registers a new instance under the
// same ID; that instance never dispatched the article, so the result must
// reach none of the failure marking, early-abort accounting, stats, file
// registration or assembler writes below. handleResult asks before either
// handler runs, and the handlers then act on res.Job rather than looking the
// ID up again.
func (p *pipeline) isCurrent(res *downloader.ArticleResult) bool {
	cur, ok := p.dispatcher.Job(res.JobID())
	return ok && cur == res.Job
}

func (p *pipeline) handleFailureResult(ctx context.Context, res *downloader.ArticleResult) {
	res.Release()
	if errors.Is(res.Err, downloader.ErrNoServersLeft) ||
		!isRetryableDownloaderError(res.Err) {
		// Terminal failure: all servers exhausted, or an unrecoverable
		// decode error.
		p.log.Warn("article permanently failed",
			"job", res.JobID(), "msgid", res.MessageID, "file", res.Subject, "err", res.Err)

		// An unregistered file cannot take the failure: the assembler resolves
		// it through the same fileInfo map and would raise a retryable
		// "resolve" write fault, stalling a job with no storage problem. The
		// article goes back to Outstanding, as handleSuccessResult does, and
		// is NOT marked failed first: the file may yet be registered by
		// another result, and a failure it never receives is a part its
		// TotalParts count can never reach.
		if err := p.registerFile(res.Job, res.FileIdx); err != nil {
			p.log.Warn("register file for a permanent failure failed, returning to dispatch pool",
				"job", res.JobID(), "fileidx", res.FileIdx, "msgid", res.MessageID, "err", err)
			p.clearEmitted(res)
			return
		}

		// Record the permanent failure directly. R10 keeps this off the write
		// path deliberately: a permanent failure asserts nothing about disk,
		// so there is no write for it to be ordered after, and losing one in
		// a crash costs a re-attempt that fails again. Only the success
		// direction needs bytes behind it.
		//
		// markDone and markFailed are both first-writer-wins on the same bit,
		// so this needs no ordering with the assembler's writes, which mark
		// an article Done through the recorder (Job.MarkArticleWritten).
		// Without this call nothing records the failure at all, and a job whose every
		// article failed finishes as Completed with an empty fail message.
		if err := res.Job.MarkArticleFailed(int(res.ArtIdx), job.WholeArticle); err != nil {
			p.log.Warn("record permanent article failure",
				"job", res.JobID(), "msgid", res.MessageID, "err", err)
		}

		// The article still goes to the assembler, which counts it toward the
		// file's part total so the file can complete with a hole in it. It
		// writes nothing and acks nothing.
		writeErr := p.assembler.WriteArticle(ctx, refFor(res), assembler.WriteRequest{
			FatalErr: res.Err,
		})
		// If the assembler couldn't accept the request, clear Emitted
		// so the dispatcher can retry the article.
		if writeErr != nil && !errors.Is(writeErr, context.Canceled) {
			p.log.Warn("write fatal article failed, returning to dispatch pool",
				"job", res.JobID(), "msgid", res.MessageID, "err", writeErr)
			p.clearEmitted(res)
		}
		telemetry.ArticlesFailed.Add(1)

		// Early abort: if the first batch of articles is mostly
		// failures, the job is likely DMCA'd or expired. Abort now
		// to save bandwidth.
		if res.Job.CheckEarlyAbort() {
			p.log.Warn("early abort: 80%+ of first articles failed, job appears DMCA'd/expired",
				"job", res.JobID())
			if p.onJobHopeless != nil {
				p.onJobHopeless(res.JobID())
			}
		}
	} else {
		// Retryable error (connection drop, 430 from one server).
		// Clear the Emitted flag so the dispatcher re-dispatches this
		// article on the next pass.
		p.log.Debug("fetch error, returning to dispatch pool",
			"job", res.JobID(), "msgid", res.MessageID, "server", res.ServerName, "err", res.Err)
		p.clearEmitted(res)
		telemetry.ArticlesRetried.Add(1)
	}
}

// clearEmitted returns res's article to the dispatch pool by clearing its
// emitted bit on res's own instance. A refusal is logged at Error: it means
// the instance has no progress or the result's index is outside it, and a
// result is only made for an article dispatched from that progress.
func (p *pipeline) clearEmitted(res *downloader.ArticleResult) {
	if err := res.Job.ClearArticleEmitted(int(res.ArtIdx)); err != nil {
		p.log.Error("return article to the dispatch pool",
			"job", res.JobID(), "msgid", res.MessageID, "err", err)
	}
}

func (p *pipeline) handleSuccessResult(ctx context.Context, res *downloader.ArticleResult) {
	// Guarantee reclamation on all exit paths. The assembler takes ownership
	// of res.Data, and of giving its bytes back to the downloader's budget
	// (WriteRequest.Release), on a successful WriteArticle; on every other
	// path (early returns, write failures) the result is released here, or
	// the budget would stay charged for an article nothing holds.
	bufferConsumed := false
	defer func() {
		if !bufferConsumed {
			res.Release()
		}
	}()

	// Record download stats
	_ = res.Job.MarkJobStarted(time.Now())                    //nolint:errcheck // telemetry; a non-resident job, its one refusal, loses only this stamp
	_ = res.Job.RecordDownload(res.ServerName, len(res.Data)) //nolint:errcheck // telemetry; a non-resident job, its one refusal, loses only these stats

	p.log.Debug("decoded article received",
		"job", res.JobID(), "msgid", res.MessageID,
		"offset", res.Offset, "bytes", len(res.Data))

	if err := p.registerFile(res.Job, res.FileIdx); err != nil {
		p.log.Warn("register file failed",
			"job", res.JobID(), "fileidx", res.FileIdx, "err", err)
		p.clearEmitted(res)
		return
	}

	// NOTHING durable is recorded here, and that is the change #389 and #421
	// were both instances of.
	//
	// This site used to append a Class A fact — the article's decoded offset,
	// length and CRC — before handing the bytes to the assembler, with no
	// ordering against the write and no way to take it back. So an article
	// the assembler then REJECTED held a record with no bytes behind it
	// (#389), and a bogus `=ypart begin=` was recorded permanently because
	// the append was INSERT OR IGNORE and re-fetching, the one thing that
	// would have corrected it, was the exact case it ignored (#421).
	//
	// The offset, length and CRC are still discovered here and are still not
	// knowable from the NZB — they come from the yEnc =ypart header inside
	// the body, so the article-to-byte-range map is itself downloaded data.
	// They travel with the article through the assembler, which reports
	// them through OnArticleWritten once the write returned nil, and the
	// recorder records them.

	writeErr := p.assembler.WriteArticle(ctx, refFor(res), assembler.WriteRequest{
		Offset:  res.Offset,
		Data:    res.Data,
		CRC32:   res.CRC,
		Release: res.ReleaseHeld,
	})
	if writeErr != nil && !errors.Is(writeErr, context.Canceled) {
		p.log.Warn("write article failed, returning to dispatch pool",
			"job", res.JobID(), "msgid", res.MessageID, "err", writeErr)
		p.clearEmitted(res)
	} else if writeErr == nil {
		bufferConsumed = true // assembler owns the buffer now
		telemetry.ArticlesWritten.Add(1)
	}
}

// registerFile ensures the assembler knows the destination path and part
// count for a file before any of its articles are written. Subsequent calls are
// no-ops. It names the file from the manifest's FileSubject, the filename the
// NZB parser already extracted from the subject line, so par2 files already
// have their .par2 extension when the repair stage scans the directory.
//
// j is the instance whose result is being handled, not a lookup of its ID:
// handleResult has already dropped a result for any other instance.
//
// A rename accepted while a file is being resolved makes the attempt fail
// with job.ErrNameChanged, and registerFile resolves the file again under the
// new name, for at most registerNameAttempts attempts.
func (p *pipeline) registerFile(j *job.Job, fileIdx int) error {
	jobID := j.ID()
	key := fileKey{jobID: jobID, fileIdx: fileIdx}

	p.mu.RLock()
	_, exists := p.fileInfo[key]
	p.mu.RUnlock()
	if exists {
		return nil
	}

	m, err := j.Manifest()
	if err != nil {
		return fmt.Errorf("manifest for job %q: %w", jobID, err)
	}
	if fileIdx < 0 || fileIdx >= m.NumFiles() {
		return fmt.Errorf("fileIdx %d out of range for job with %d files", fileIdx, m.NumFiles())
	}
	for attempt := 1; ; attempt++ {
		err := p.registerFileAttempt(j, m, fileIdx)
		if !errors.Is(err, job.ErrNameChanged) || attempt == registerNameAttempts {
			return err
		}
	}
}

// registerNameAttempts bounds registerFile's attempts at one file. An attempt
// fails with job.ErrNameChanged only when a rename was accepted inside its
// window, so each further failure needs another rename to land within one
// more resolution. Renames come from the API, so a run of three is not
// expected; the bound stops a loop rather than shaping behaviour. The last
// failure is returned, and the calling handler (handleSuccessResult or
// handleFailureResult) then returns the article to the dispatch pool, which
// fetches it again.
const registerNameAttempts = 3

// registerFileAttempt is one attempt of registerFile, for a file index already
// checked against m.
func (p *pipeline) registerFileAttempt(j *job.Job, m *job.Manifest, fileIdx int) error {
	jobID := j.ID()
	key := fileKey{jobID: jobID, fileIdx: fileIdx}

	// One read of both: a file with a filename makes DownloadBegun true, so
	// Job.SetName refuses every later rename and name is final.
	name, filename := j.FileLocation(fileIdx)
	if p.nameReadHook != nil {
		p.nameReadHook(j)
	}

	p.mu.Lock()

	// Double-check under the write lock — another goroutine may have won
	// the race between RUnlock and Lock.
	if _, exists := p.fileInfo[key]; exists {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	// --- No lock held below this line ---

	// Use the filename stored in Subject — the NZB parser already ran
	// ExtractFilenameFromSubject + SanitizeFilename at parse time
	// (parser.go convertFile), so Subject IS the clean filename with
	// its real extension (.par2, .rar, etc.). Running the extraction
	// regex again would break filenames containing characters like '&'
	// that aren't in the basic-filename regex's character class.
	//
	// Both resolvers put every file of a job in filepath.Join(downloadDir,
	// job name), the directory postproc derives when scanning for par2 sets.
	var loc jobFile
	if filename != "" {
		// The name this file was first written under, verbatim: the restart
		// verifier resolves it the same way (jobFileLocation).
		var err error
		if loc, err = p.jobFileLocation(name, filename); err != nil {
			return fmt.Errorf("recorded filename: %w", err)
		}
	} else {
		// First time resolving this file; registerFile records the name below.
		loc = p.newJobFileLocation(name, m.FileSubject(fileIdx))
	}

	// Count only unfinished articles — on resume/retry, already-done
	// articles won't be re-dispatched, so TotalParts must match the
	// number the assembler will actually receive.
	totalParts, err := j.CountUnfinishedArticles(fileIdx)
	if err != nil {
		return fmt.Errorf("count unfinished articles: %w", err)
	}
	if p.countedHook != nil {
		p.countedHook(j)
	}

	// Owned seeds the writer's ranges with the file's verified rows, so an
	// arrival that intersects one is refused as it would be had this process
	// written it, and the completion truncate bounds over them.
	info := assembler.FileInfo{
		Dir:          loc.Dir,
		Name:         loc.Name,
		TotalParts:   totalParts,
		ExpectedSize: preallocSize(m, fileIdx),
		Owned:        ownedRanges(j.FileRows(fileIdx)),
	}

	p.mu.Lock()
	// Third check: another goroutine may have raced us and already
	// registered this file while we were doing unlocked disk I/O above.
	// uniqueJobFileName only consults on-disk state (never a file this
	// process creates), and the winner recorded its filename under the
	// job's current name (RecordFileFilename), so its resolved path is the
	// one to keep — deferring to it is correct, not just harmless.
	if _, exists := p.fileInfo[key]; exists {
		p.mu.Unlock()
		return nil
	}
	if filename == "" {
		// RecordFileFilename writes the progress record alone, so it records
		// the filename on a job evicted or removed since CountUnfinishedArticles
		// read the manifest: both drop only the manifest. A rename since name
		// was read is job.ErrNameChanged, which registerFile retries. Any
		// refusal aborts with nothing cached, so the next article for the file
		// registers it again rather than taking the cached entry of a file
		// whose filename was never recorded.
		if err := j.RecordFileFilename(fileIdx, name, loc.Name); err != nil {
			p.mu.Unlock()
			return fmt.Errorf("record file filename: %w", err)
		}
		if p.markDirty != nil {
			p.markDirty(j, fileIdx)
		}
	}
	p.fileInfo[key] = info
	p.mu.Unlock()
	// --- No lock held below this line ---
	p.log.Debug("registered file",
		"job", jobID, "fileidx", fileIdx, "path", info.Path(), "parts", info.TotalParts)

	return nil
}

// resolveFileInfo is the FileInfo resolver handed to the assembler. It
// returns the cached entry for (jobID, fileIdx). The pipeline populates
// the entry before enqueuing the corresponding WriteArticle, so the
// assembler worker always sees it by the time it calls this.
func (p *pipeline) resolveFileInfo(jobID string, fileIdx int) (assembler.FileInfo, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	info, ok := p.fileInfo[fileKey{jobID: jobID, fileIdx: fileIdx}]
	if !ok {
		return assembler.FileInfo{}, fmt.Errorf("no file info for %s[%d]", jobID, fileIdx)
	}
	return info, nil
}

// ownedRanges is the byte ranges of rows, for FileInfo.Owned.
func ownedRanges(rows []durability.WrittenRow) []assembler.Range {
	if len(rows) == 0 {
		return nil
	}
	out := make([]assembler.Range, len(rows))
	for i, r := range rows {
		out[i] = assembler.Range{Off: r.Offset, Len: r.Length}
	}
	return out
}

// forgetFile removes one file's cached fileInfo entry, so the next article for
// it registers the file again: a fresh part count and owned set. Called when a
// file is untrusted.
func (p *pipeline) forgetFile(jobID string, fileIdx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.fileInfo, fileKey{jobID: jobID, fileIdx: fileIdx})
}

// forgetJob removes all cached fileInfo entries for jobID. Called when
// a job is fully assembled (or cancelled) to prevent the map from growing
// unboundedly across many completed downloads. Safe for concurrent use.
func (p *pipeline) forgetJob(jobID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.fileInfo {
		if key.jobID == jobID {
			delete(p.fileInfo, key)
		}
	}
}
