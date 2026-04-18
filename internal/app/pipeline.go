package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/hobeone/sabnzbd-go/internal/assembler"
	"github.com/hobeone/sabnzbd-go/internal/decoder"
	"github.com/hobeone/sabnzbd-go/internal/downloader"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

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
		if errors.Is(res.Err, downloader.ErrNoServersLeft) {
			p.log.Warn("article permanently failed, marking in queue",
				"job", res.JobID, "msgid", res.MessageID)

			first, err := p.queue.MarkArticleFailed(res.JobID, res.MessageID)
			if err != nil {
				p.log.Warn("failed to mark article failed in queue",
					"job", res.JobID, "msgid", res.MessageID, "err", err)
				return
			}
			if !first {
				return // Already processed this failure
			}

			// Find the job/file to get a fallback filename if not yet registered
			job, err := p.queue.Get(res.JobID)
			var filename string
			if err == nil && res.FileIdx >= 0 && res.FileIdx < len(job.Files) {
				filename = job.Files[res.FileIdx].Subject
			} else {
				filename = "unknown_failed_part"
			}

			if err := p.registerFile(res.JobID, res.FileIdx, filename); err != nil {
				p.log.Warn("register fallback file failed",
					"job", res.JobID, "fileidx", res.FileIdx, "err", err)
			}

			// Notify assembler of the failure so it can count the part for completion
			_ = p.assembler.WriteArticle(ctx, assembler.WriteRequest{
				JobID:    res.JobID,
				FileIdx:  res.FileIdx,
				FatalErr: res.Err,
			})
		} else {
			p.log.Info("fetch error",
				"job", res.JobID, "msgid", res.MessageID, "server", res.ServerName, "err", res.Err)
		}
		return
	}

	article, err := decoder.DecodeArticle(res.Body)
	if err != nil {
		p.log.Warn("decode error",
			"job", res.JobID, "msgid", res.MessageID, "err", err)
		return
	}

	p.log.Debug("decoded article",
		"job", res.JobID, "msgid", res.MessageID, "file", article.Filename,
		"offset", article.Offset, "bytes", len(article.Data))

	if err := p.registerFile(res.JobID, res.FileIdx, article.Filename); err != nil {
		p.log.Warn("register file failed",
			"job", res.JobID, "fileidx", res.FileIdx, "err", err)
		return
	}

	writeErr := p.assembler.WriteArticle(ctx, assembler.WriteRequest{
		JobID:   res.JobID,
		FileIdx: res.FileIdx,
		Offset:  article.Offset,
		Data:    article.Data,
	})
	if writeErr != nil && !errors.Is(writeErr, context.Canceled) {
		p.log.Warn("write article failed",
			"job", res.JobID, "msgid", res.MessageID, "err", writeErr)
	}
}

// registerFile records the target path and expected part count for a file
// on first encounter. Subsequent calls for the same (jobID, fileIdx) are
// no-ops. The filename is taken as filepath.Base to prevent yEnc headers
// that contain a path from escaping downloadDir.
func (p *pipeline) registerFile(jobID string, fileIdx int, yencName string) error {
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

	// filepath.Base strips any path components from the yEnc name= field,
	// preventing a malicious article from writing outside downloadDir via
	// a "../../etc/passwd" style filename.
	filename := filepath.Base(yencName)
	if filename == "" || filename == "." || filename == "/" {
		return fmt.Errorf("invalid filename %q", yencName)
	}

	info := assembler.FileInfo{
		Path:       filepath.Join(p.downloadDir, job.Name, filename),
		TotalParts: len(job.Files[fileIdx].Articles),
	}

	p.mu.Lock()
	// Double-check under the write lock — another goroutine may have won
	// the race between RUnlock and Lock.
	if _, exists := p.fileInfo[key]; !exists {
		p.fileInfo[key] = info
		p.log.Debug("registered file",
			"job", jobID, "fileidx", fileIdx, "path", info.Path, "parts", info.TotalParts)
	}
	p.mu.Unlock()
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
