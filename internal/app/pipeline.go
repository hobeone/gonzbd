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
	queue       *queue.Queue
	assembler   *assembler.Assembler
	completions <-chan *downloader.ArticleResult
	downloadDir string

	mu       sync.RWMutex
	fileInfo map[fileKey]assembler.FileInfo
}

// run is the pipeline's sole goroutine. Returns when ctx is cancelled or
// the Completions channel is closed (which the downloader does on Stop).
func (p *pipeline) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case res, ok := <-p.completions:
			if !ok {
				return
			}
			p.handleResult(ctx, res)
		}
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
	if res.Err != nil {
		slog.Debug("pipeline: fetch error",
			"job", res.JobID, "msgid", res.MessageID, "server", res.ServerName, "err", res.Err)
		return
	}

	article, err := decoder.DecodeArticle(res.Body)
	if err != nil {
		slog.Warn("pipeline: decode error",
			"job", res.JobID, "msgid", res.MessageID, "err", err)
		return
	}

	if err := p.registerFile(res.JobID, res.FileIdx, article.Filename); err != nil {
		slog.Warn("pipeline: register file",
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
		slog.Warn("pipeline: write article",
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
