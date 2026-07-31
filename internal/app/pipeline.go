package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/queue"
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
//   - Dial errors / connection resets / I/O timeouts
//
// Terminal errors (other decode failures, ErrAuthRejected, etc.) return false
// and should be routed through the assembler's FatalErr path for failure
// accounting.
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

// pipeline plumbs Downloader.Completions() → decoder → assembler.
// The reader goroutine (run) fans out ArticleResults to a pool of worker
// goroutines that call handleResult concurrently. The fileInfo map is
// populated by workers and read by the assembler worker via resolveFileInfo,
// so access is protected by mu.
type pipeline struct {
	log         *slog.Logger
	queue       *queue.Queue
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

	// ctx is the context passed to run(); stored so setCompletions can
	// avoid blocking forever if run() has already exited.
	ctx context.Context

	// updateCh receives a swap request with a done channel for ack.
	updateCh chan completionSwap

	mu       sync.RWMutex
	fileInfo map[fileKey]assembler.FileInfo
}

// completionSwap bundles a new completions channel with a done channel.
// The run loop closes done after fully switching, letting setCompletions
// block until the old channel is drained.
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
						work <- res
					default:
						goto drained
					}
				}
			}
		drained:
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
			work <- res
		}
	}
}

// setCompletions swaps the source of ArticleResults. Blocks until the
// pipeline's run loop has drained any remaining results from the old
// channel and acknowledged the swap.
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

// handleResult processes one downloader output: decodes the body if there
// was no fetch error, registers file info on first encounter, and hands the
// decoded part to the assembler for pwrite.
//
// Errors at every stage are currently logged and discarded. Full fail-job
// handling (marking the NzbObject failed, pushing to post-processor) is
// Phase 5's problem.
func (p *pipeline) handleResult(ctx context.Context, res *downloader.ArticleResult) {
	telemetry.ArticlesReceived.Add(1)
	if p.onHeartbeat != nil {
		p.onHeartbeat()
	}

	if res.Err != nil {
		p.handleFailureResult(ctx, res)
	} else {
		p.handleSuccessResult(ctx, res)
	}
}

func (p *pipeline) handleFailureResult(ctx context.Context, res *downloader.ArticleResult) {
	if res.Data != nil {
		decoder.PutBuffer(res.Data)
	}
	if errors.Is(res.Err, downloader.ErrNoServersLeft) ||
		!isRetryableDownloaderError(res.Err) {
		// Terminal failure (all servers exhausted or unrecoverable
		// decode error). Hand to the assembler so it can mark the
		// article Failed in the queue.
		p.log.Warn("article permanently failed, handing to assembler",
			"job", res.JobID, "msgid", res.MessageID, "file", res.Subject, "err", res.Err)

		if err := p.registerFile(res.JobID, res.FileIdx); err != nil {
			p.log.Warn("register fallback file failed",
				"job", res.JobID, "fileidx", res.FileIdx, "err", err)
		}

		// The assembler marks the article Failed in the queue (with dup
		// suppression) so failure and completion accounting stay ordered
		// with file writes on the single worker goroutine.
		writeErr := p.assembler.WriteArticle(ctx, assembler.WriteRequest{
			JobID:     res.JobID,
			FileIdx:   res.FileIdx,
			ArtIdx:    res.ArtIdx,
			MessageID: res.MessageID,
			FatalErr:  res.Err,
		})
		// If the assembler couldn't accept the request, clear Emitted
		// so the dispatcher can retry the article.
		if writeErr != nil && !errors.Is(writeErr, context.Canceled) {
			p.log.Warn("write fatal article failed, returning to dispatch pool",
				"job", res.JobID, "msgid", res.MessageID, "err", writeErr)
			_ = p.queue.ClearArticleEmittedByIdx(res.JobID, res.ArtIdx)
		}
		telemetry.ArticlesFailed.Add(1)

		// Early abort: if the first batch of articles is mostly
		// failures, the job is likely DMCA'd or expired. Abort now
		// to save bandwidth.
		if p.queue.CheckEarlyAbort(res.JobID) {
			p.log.Warn("early abort: 80%+ of first articles failed, job appears DMCA'd/expired",
				"job", res.JobID)
			if p.onJobHopeless != nil {
				p.onJobHopeless(res.JobID)
			}
		}
	} else {
		// Retryable error (connection drop, 430 from one server).
		// Clear the Emitted flag so the dispatcher re-dispatches this
		// article on the next pass.
		p.log.Debug("fetch error, returning to dispatch pool",
			"job", res.JobID, "msgid", res.MessageID, "server", res.ServerName, "err", res.Err)
		if err := p.queue.ClearArticleEmittedByIdx(res.JobID, res.ArtIdx); err != nil {
			p.log.Debug("clear emitted failed, this typically happens when a job has already been stopped", "job", res.JobID, "msgid", res.MessageID, "err", err)
		}
		telemetry.ArticlesRetried.Add(1)
	}
}

func (p *pipeline) handleSuccessResult(ctx context.Context, res *downloader.ArticleResult) {
	// Guarantee buffer reclamation on all exit paths. The assembler takes
	// ownership of res.Data on successful WriteArticle; on every other path
	// (early returns, write failures) the buffer must be returned to the pool
	// to avoid GC pressure from leaked sync.Pool buffers.
	bufferConsumed := false
	defer func() {
		if !bufferConsumed && res.Data != nil {
			decoder.PutBuffer(res.Data)
		}
	}()

	// Record download stats
	if err := p.queue.MarkJobStarted(res.JobID, time.Now()); err != nil {
		p.log.Debug("mark job started failed (job likely removed)", "job", res.JobID, "err", err)
		_ = p.queue.ClearArticleEmittedByIdx(res.JobID, res.ArtIdx)
		return
	}
	if err := p.queue.RecordDownload(res.JobID, res.ServerName, len(res.Data)); err != nil {
		p.log.Debug("record download failed (job likely removed)", "job", res.JobID, "err", err)
		_ = p.queue.ClearArticleEmittedByIdx(res.JobID, res.ArtIdx)
		return
	}

	p.log.Debug("decoded article received",
		"job", res.JobID, "msgid", res.MessageID,
		"offset", res.Offset, "bytes", len(res.Data))

	if err := p.registerFile(res.JobID, res.FileIdx); err != nil {
		p.log.Warn("register file failed",
			"job", res.JobID, "fileidx", res.FileIdx, "err", err)
		_ = p.queue.ClearArticleEmittedByIdx(res.JobID, res.ArtIdx)
		return
	}

	writeErr := p.assembler.WriteArticle(ctx, assembler.WriteRequest{
		JobID:     res.JobID,
		FileIdx:   res.FileIdx,
		ArtIdx:    res.ArtIdx,
		MessageID: res.MessageID,
		Offset:    res.Offset,
		Data:      res.Data,
		CRC:       res.CRC,
	})
	if writeErr != nil && !errors.Is(writeErr, context.Canceled) {
		p.log.Warn("write article failed, returning to dispatch pool",
			"job", res.JobID, "msgid", res.MessageID, "err", writeErr)
		_ = p.queue.ClearArticleEmittedByIdx(res.JobID, res.ArtIdx)
	} else if writeErr == nil {
		bufferConsumed = true // assembler owns the buffer now
		telemetry.ArticlesWritten.Add(1)
	}
}

// registerFile records the target path and expected part count for a file
// on first encounter. Subsequent calls for the same (jobID, fileIdx) are
// no-ops. It extracts the real filename from the NZB subject line (matching
// Python SABnzbd's approach) so par2 files already have their .par2
// extension when the repair stage scans the directory.
func (p *pipeline) registerFile(jobID string, fileIdx int) error {
	key := fileKey{jobID: jobID, fileIdx: fileIdx}

	p.mu.RLock()
	_, exists := p.fileInfo[key]
	p.mu.RUnlock()
	if exists {
		return nil
	}

	snap, err := p.queue.SnapshotJob(jobID)
	if err != nil {
		return fmt.Errorf("queue lookup: job %q: %w", jobID, err)
	}
	m := snap.Manifest()
	if m == nil {
		return fmt.Errorf("queue lookup: manifest for job %q is nil", jobID)
	}
	if fileIdx < 0 || fileIdx >= m.NumFiles() {
		return fmt.Errorf("fileIdx %d out of range for job with %d files", fileIdx, m.NumFiles())
	}

	p.mu.Lock()

	// Double-check under the write lock — another goroutine may have won
	// the race between RUnlock and Lock.
	if _, exists := p.fileInfo[key]; exists {
		p.mu.Unlock()
		return nil
	}

	// Use the filename stored in Subject — the NZB parser already ran
	// ExtractFilenameFromSubject + SanitizeFilename at parse time
	// (parser.go convertFile), so Subject IS the clean filename with
	// its real extension (.par2, .rar, etc.). Running the extraction
	// regex again would break filenames containing characters like '&'
	// that aren't in the basic-filename regex's character class.
	// Compute the job directory once from the already-sanitized job name
	// (queue.NewJob runs SanitizeFolderName at admission). This guarantees
	// every file in a job lands in the same directory — postproc derives
	// the same path via filepath.Join(DownloadDir, job.Name) when scanning
	// for par2 sets.
	jobDir := filepath.Join(p.downloadDir, snap.Name)
	p.mu.Unlock()
	// --- No lock held below this line ---
	// jobDir snapshots p.downloadDir; p.sanitize is set once at construction
	// and never mutated, so both are safe to use unlocked below.

	var path string
	filename := snap.Progress().FileFilename(fileIdx)
	if filename != "" {
		// Use the already-resolved and persisted filename directly, preventing
		// duplicate renaming across daemon restarts.
		path = fsutil.JoinSafe(jobDir, "", filename, p.sanitize)
	} else {
		// First time resolving this file. GetUniqueFilename appends ".1", ".2" etc.
		// when the path already exists on disk (e.g. naming collisions).
		// This is real disk I/O (a Stat loop) and must not run under p.mu.
		candidate := m.FileSubject(fileIdx)
		path = fsutil.GetUniqueFilename(
			fsutil.JoinSafe(jobDir, "", candidate, p.sanitize))
	}

	// Count only unfinished articles — on resume/retry, already-done
	// articles won't be re-dispatched, so TotalParts must match the
	// number the assembler will actually receive.
	totalParts, err := p.queue.CountUnfinishedArticles(jobID, fileIdx)
	if err != nil {
		return fmt.Errorf("count unfinished articles: %w", err)
	}

	info := assembler.FileInfo{
		Path:               path,
		TotalParts:         totalParts,
		ExpectedSize:       m.FileBytes(fileIdx),
		InitialWriteCursor: snap.Progress().FileWriteCursor(fileIdx),
	}

	p.mu.Lock()
	// Third check: another goroutine may have raced us and already
	// registered this file while we were doing unlocked disk I/O above.
	// GetUniqueFilename only consults on-disk state (never a file this
	// process creates), so a concurrent winner's resolved path is
	// identical to ours — deferring to it is correct, not just harmless.
	if _, exists := p.fileInfo[key]; exists {
		p.mu.Unlock()
		return nil
	}
	if filename == "" {
		resolvedFilename := filepath.Base(path)
		if err := p.queue.SetFileFilename(jobID, fileIdx, resolvedFilename); err != nil {
			p.mu.Unlock()
			return fmt.Errorf("set file filename: %w", err)
		}
	}
	p.fileInfo[key] = info
	p.mu.Unlock()
	// --- No lock held below this line ---
	p.log.Debug("registered file",
		"job", jobID, "fileidx", fileIdx, "path", info.Path, "parts", info.TotalParts)

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
