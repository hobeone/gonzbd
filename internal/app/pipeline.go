package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/assembler"
	"github.com/hobeone/sabnzbd-go/internal/downloader"
	"github.com/hobeone/sabnzbd-go/internal/fsutil"
	"github.com/hobeone/sabnzbd-go/internal/nntp"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// isRetryableDownloaderError returns true for transient errors where the
// dispatcher will retry the article (on the same or another server):
//   - 430 / ErrNoArticle: not on this server, try fallback
//   - Dial errors: server temporarily unreachable
//   - Connection-level I/O failures
//
// Terminal errors (decode failures, etc.) return false and should be
// routed through the assembler's FatalErr path for failure accounting.
func isRetryableDownloaderError(err error) bool {
	if errors.Is(err, nntp.ErrNoArticle) {
		return true
	}
	// Dial errors are wrapped as "dial: <inner>" by handleRequest.
	msg := err.Error()
	return strings.HasPrefix(msg, "dial:") ||
		strings.Contains(msg, "connection") ||
		strings.Contains(msg, "i/o timeout")
}

// fileKey uniquely identifies a file within a job.
type fileKey struct {
	jobID   string
	fileIdx int
}

// pipeline plumbs Downloader.Completions() → decoder → assembler.
// Exactly one goroutine runs pipeline.run; all public methods used by that
// goroutine are single-writer. The fileInfo map is the exception — it is
// populated by run and read concurrently by the assembler worker via
// resolveFileInfo, so access is protected by mu.
type pipeline struct {
	log         *slog.Logger
	queue       *queue.Queue
	assembler   *assembler.Assembler
	completions <-chan *downloader.ArticleResult
	downloadDir string
	sanitize    fsutil.SanitizeOptions

	// updateCh receives a new completions channel to switch to.
	updateCh chan (<-chan *downloader.ArticleResult)

	mu       sync.RWMutex
	fileInfo map[fileKey]assembler.FileInfo
}

// run is the pipeline's sole goroutine. Returns when ctx is cancelled.
func (p *pipeline) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case newCh := <-p.updateCh:
			p.completions = newCh
		case res, ok := <-p.completions:
			if !ok {
				// Downloader stopped; wait for a new channel or cancellation.
				// Set completions to nil so we don't busy-spin on a closed channel.
				p.log.Info("Downloader stopped, waiting for new channel")
				p.completions = nil
				continue
			}
			p.handleResult(ctx, res)
		}
	}
}

// setCompletions swaps the source of ArticleResults. Safe for concurrent use.
func (p *pipeline) setCompletions(ch <-chan *downloader.ArticleResult) {
	p.updateCh <- ch
}

// handleResult processes one downloader output: decodes the body if there
// was no fetch error, registers file info on first encounter, and hands the
// decoded part to the assembler for pwrite.
//
// Errors at every stage are currently logged and discarded. Full fail-job
// handling (marking the NzbObject failed, pushing to post-processor) is
// Phase 5's problem.
func (p *pipeline) handleResult(ctx context.Context, res *downloader.ArticleResult) {
	if res.Err != nil {
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
				MessageID: res.MessageID,
				FatalErr:  res.Err,
			})
			// If the assembler couldn't accept the request, clear Emitted
			// so the dispatcher can retry the article.
			if writeErr != nil && !errors.Is(writeErr, context.Canceled) {
				p.log.Warn("write fatal article failed, returning to dispatch pool",
					"job", res.JobID, "msgid", res.MessageID, "err", writeErr)
				_ = p.queue.ClearArticleEmitted(res.JobID, res.MessageID)
			}
		} else {
			// Retryable error (connection drop, 430 from one server).
			// Clear the Emitted flag so the dispatcher re-dispatches this
			// article on the next pass.
			p.log.Info("fetch error, returning to dispatch pool",
				"job", res.JobID, "msgid", res.MessageID, "server", res.ServerName, "err", res.Err)
			if err := p.queue.ClearArticleEmitted(res.JobID, res.MessageID); err != nil {
				p.log.Warn("clear emitted failed", "job", res.JobID, "msgid", res.MessageID, "err", err)
			}
		}
		return
	}

	// Record download stats
	p.queue.MarkJobStarted(res.JobID, time.Now())
	p.queue.RecordDownload(res.JobID, res.ServerName, len(res.Data))

	p.log.Debug("decoded article received",
		"job", res.JobID, "msgid", res.MessageID,
		"offset", res.Offset, "bytes", len(res.Data))

	if err := p.registerFile(res.JobID, res.FileIdx); err != nil {
		p.log.Warn("register file failed",
			"job", res.JobID, "fileidx", res.FileIdx, "err", err)
		return
	}

	writeErr := p.assembler.WriteArticle(ctx, assembler.WriteRequest{
		JobID:     res.JobID,
		FileIdx:   res.FileIdx,
		MessageID: res.MessageID,
		Offset:    res.Offset,
		Data:      res.Data,
	})
	if writeErr != nil && !errors.Is(writeErr, context.Canceled) {
		p.log.Warn("write article failed, returning to dispatch pool",
			"job", res.JobID, "msgid", res.MessageID, "err", writeErr)
		_ = p.queue.ClearArticleEmitted(res.JobID, res.MessageID)
	}
}

// registerFile records the target path and expected part count for a file
// on first encounter. Subsequent calls for the same (jobID, fileIdx) are
// no-ops. It uses a deterministic temporary path based on the JobID and
// file index to handle obfuscated and messy data robustly.
func (p *pipeline) registerFile(jobID string, fileIdx int) error {
	key := fileKey{jobID: jobID, fileIdx: fileIdx}

	p.mu.RLock()
	_, exists := p.fileInfo[key]
	p.mu.RUnlock()
	if exists {
		return nil
	}

	job, err := p.queue.Get(jobID)
	if err != nil {
		return fmt.Errorf("queue lookup: %w", err)
	}
	if fileIdx < 0 || fileIdx >= len(job.Files) {
		return fmt.Errorf("fileIdx %d out of range for job with %d files", fileIdx, len(job.Files))
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check under the write lock — another goroutine may have won
	// the race between RUnlock and Lock.
	if _, exists := p.fileInfo[key]; exists {
		return nil
	}

	// Use job Name and file index for a human-readable and robust path.
	// Final naming of files is deferred until the post-processing (PAR2) phase.
	// We use JoinSafe to ensure the absolute path does not exceed OS limits.
	path := fsutil.JoinSafe(p.downloadDir, job.Name, fmt.Sprintf("%04d.nzf", fileIdx), p.sanitize)

	// Count only unfinished articles — on resume/retry, already-done
	// articles won't be re-dispatched, so TotalParts must match the
	// number the assembler will actually receive.
	totalParts, err := p.queue.CountUnfinishedArticles(jobID, fileIdx)
	if err != nil {
		return fmt.Errorf("count unfinished articles: %w", err)
	}

	info := assembler.FileInfo{
		Path:       path,
		TotalParts: totalParts,
	}

	p.fileInfo[key] = info
	p.log.Debug("registered temporary file",
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
