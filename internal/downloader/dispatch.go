package downloader

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/nntp"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// dispatchPass walks the queue once and tries to feed every not-yet-
// done article into an eligible server's work channel.
//
// Eligibility rules for a (article, server) pair:
//  1. Server is Enable && Active(now) (not under penalty / deactivated).
//  2. The article has not already been definitively rejected by this
//     server (try-list miss).
//
// Sending is non-blocking: if the server's work channel is full, the
// dispatcher skips to the next server for that article. If no server
// can accept the article this pass, the article is simply left alone;
// a future signalDispatch (worker completion) or queue.Notify (queue
// mutation) will trigger another pass.
//
// The pass holds no locks across queue iteration: queue.List returns
// a snapshot slice and article access is read-only (we dispatch work,
// we don't mutate state here — success/failure handling is in
// handleRequest).
func (d *Downloader) dispatchPass(ctx context.Context) {
	if d.paused.Load() || d.queue.IsPaused() {
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	now := time.Now()

	dispatched := 0
	hopelessJobs := make(map[string]struct{})
	// exhausted accumulates articles that had no eligible server this
	// pass. Emitting their ErrNoServersLeft results inline would require
	// sending on the completions channel while tryMu and the queue
	// RLock (from ForEachUnfinishedArticle) are both held — a deadlock
	// when the pipeline consumer itself needs the queue write lock.
	// We drain this list after the iterator returns.
	var exhausted []*articleRequest

	d.queue.ForEachUnfinishedArticle(func(a queue.UnfinishedArticle) bool {
		if a.JobStatus == constants.StatusPaused {
			return true // skip paused jobs, keep iterating
		}

		// Early Health Gate: Check if the job is beyond repair.
		if a.FailedBytes > a.Par2Bytes {
			hopelessJobs[a.JobID] = struct{}{}
			return true // Move to next job
		}

		handled, exReq := d.tryDispatch(ctx, a.JobID, a.FileIdx, a.MessageID, a.Bytes, a.Subject, now)
		if handled {
			dispatched++
		}
		if exReq != nil {
			exhausted = append(exhausted, exReq)
		}
		// Always continue — per-article send is non-blocking and
		// we want to fan out as much as will fit this pass.
		return ctx.Err() == nil
	})

	// Queue RLock and tryMu are both released here. Safe to block on
	// completions; the pipeline consumer can now take the queue write
	// lock.
	for _, req := range exhausted {
		// Mark Emitted before emitting so a concurrent dispatch pass
		// triggered by another worker's signalDispatch doesn't re-see
		// the article as dispatchable (all try-list entries would still
		// be present, so it would keep re-emitting ErrNoServersLeft in
		// a tight loop until the assembler finally marked it Failed).
		if err := d.queue.MarkArticleEmitted(req.jobID, req.messageID); err != nil {
			d.log.Warn("mark article emitted failed", "job", req.jobID, "msgid", req.messageID, "err", err)
		}
		d.emitResult(ctx, req, "", nil, ErrNoServersLeft)
	}

	// Handle hopeless jobs after the queue read-lock is released.
	for jobID := range hopelessJobs {
		d.log.Warn("job beyond repair (failed bytes > par2 bytes), marking FAILED", "job", jobID)
		if d.onJobHopeless != nil {
			d.onJobHopeless(jobID)
		} else {
			_ = d.queue.Pause(jobID) // Fallback if no callback
		}
	}
}

// tryDispatch hands the article to the first eligible server with
// spare capacity. The server is recorded in the try-list and the
// article's in-flight counter is incremented atomically with the
// send, so a later dispatch pass cannot re-send the same article
// while one is still being fetched.
//
// If the article already has an outstanding request on any server,
// tryDispatch returns immediately. Fallback to another server happens
// only after the current request resolves (via its worker's
// signalDispatch). This matches Python's sequential fallback
// semantics and avoids paying paid-bandwidth twice for the same
// article.
//
// The try-list entry is cleaned up by handleRequest: on success the
// whole article entry is removed; on retryable connection failure
// the current server is unmarked; on a definitive 430 the entry
// stays so the article falls through to the next server.
//
// Returns (handled, exhausted):
//   - handled=true means the caller should treat the article as done for
//     this pass (either because we fanned it out to a server or because
//     every server is already in its try-list).
//   - exhausted is non-nil when every server has been tried and the
//     article is permanently failed for this session. The caller must
//     emit ErrNoServersLeft for it *after* releasing any locks held
//     across the dispatchPass iteration — emitting inline would deadlock
//     the dispatcher if the completions channel is full, because the
//     consumer needs the queue write lock that dispatchPass is currently
//     holding via ForEachUnfinishedArticle.
//
// A future dispatchReady signal from any worker will bring us back to
// re-try articles that returned (false, nil).
func (d *Downloader) tryDispatch(ctx context.Context, jobID string, fileIdx int, messageID string, bytes int, subject string, now time.Time) (bool, *articleRequest) {
	key := articleKey{jobID: jobID, messageID: messageID}
	req := &articleRequest{
		jobID:     jobID,
		messageID: messageID,
		fileIdx:   fileIdx,
		bytes:     bytes,
		subject:   subject,
	}

	d.tryMu.Lock()
	defer d.tryMu.Unlock()

	if d.inFlight[key] > 0 {
		return false, nil
	}

	tried := d.tryList[key]
	anyEligible := false
	for _, srv := range d.servers {
		name := srv.Cfg().Name
		if !srv.Active(now) {
			continue
		}
		if _, already := tried[name]; already {
			continue
		}
		anyEligible = true
		ch, ok := d.workCh[name]
		if !ok {
			continue
		}
		select {
		case ch <- req:
			if tried == nil {
				tried = make(map[string]struct{})
				d.tryList[key] = tried
			}
			tried[name] = struct{}{}
			d.inFlight[key]++
			return true, nil
		case <-ctx.Done():
			return false, nil
		default:
			// server's queue is full; try next server
		}
	}

	// If we found no eligible servers to even try (all are in the
	// tryList), this article is permanently failed for this session.
	// Return the req so dispatchPass can emit ErrNoServersLeft after
	// locks are released.
	if !anyEligible {
		d.log.Warn("article failed on all servers", "msgid", messageID, "job", jobID)
		return true, req
	}

	return false, nil
}

// connWorker is one connection-owning goroutine. It lazily dials its
// *nntp.Conn on the first request and reuses it for subsequent
// fetches. On a connection-level failure the conn is closed and
// re-dialled for the next request. The goroutine exits when ctx is
// cancelled.
func (d *Downloader) connWorker(ctx context.Context, srv *Server) {
	var conn *nntp.Conn
	defer func() {
		if conn != nil {
			_ = conn.Close() //nolint:errcheck // shutdown path; close error not actionable
		}
	}()

	name := srv.Cfg().Name
	workCh := d.workCh[name]

	d.log.Info("Creating server connection", "server", srv.Cfg().Host)

	for {
		select {
		case <-ctx.Done():
			return
		case req := <-workCh:
			d.handleRequest(ctx, srv, &conn, req)
		}
	}
}

// handleRequest is the per-article workhorse. It owns the
// bookkeeping for try-lists, penalty application, and success/error
// emission. The *nntp.Conn pointer is passed by reference so the
// function can replace it with nil on connection-level failure
// (forcing a re-dial on the next call).
func (d *Downloader) handleRequest(ctx context.Context, srv *Server, connPtr **nntp.Conn, req *articleRequest) {
	defer d.signalDispatch()
	defer d.clearInFlight(req.jobID, req.messageID)

	name := srv.Cfg().Name

	if *connPtr == nil {
		d.log.Info("dialing server", "server", name, "host", srv.Cfg().Host)
		c, err := nntp.Dial(ctx, srv.Cfg(),
			nntp.WithLimiter(d.limiter),
			nntp.WithLogger(d.log),
		)
		if err != nil {
			d.log.Warn("dial failed", "server", name, "error", err)
			srv.RecordBadConnection()
			if pen := PenaltyFor(err); pen > 0 {
				d.log.Info("penalty applied", "server", name, "duration", pen)
				srv.ApplyPenalty(pen)
			}
			// Retryable: unmark so another pass can try another
			// server (or this one again after the penalty).
			d.unmarkTried(req.jobID, req.messageID, name)
			d.emitResult(ctx, req, name, nil, fmt.Errorf("dial: %w", err))
			return
		}
		d.log.Info("connected", "server", name, "ssl", c.SSLInfo())
		*connPtr = c
	}

	body, err := (*connPtr).Fetch(ctx, req.messageID)
	if err != nil {
		if errors.Is(err, nntp.ErrNoArticle) {
			d.log.Info("article not found", "server", name, "msgid", req.messageID)
			// The server definitively said no. Try-list entry is
			// retained so we won't retry here; connection is
			// healthy — reuse it.
			srv.RecordGoodConnection()
			d.emitResult(ctx, req, name, nil, err)
			return
		}
		// Connection-level failure: tear down, re-dial later.
		d.log.Warn("fetch failed", "server", name, "msgid", req.messageID, "error", err)
		_ = (*connPtr).Close() //nolint:errcheck // discarding a broken conn; underlying error already captured in err
		*connPtr = nil
		srv.RecordBadConnection()
		if pen := PenaltyFor(err); pen > 0 {
			d.log.Info("penalty applied", "server", name, "duration", pen)
			srv.ApplyPenalty(pen)
		}
		d.unmarkTried(req.jobID, req.messageID, name)
		d.emitResult(ctx, req, name, nil, err)
		return
	}

	srv.RecordGoodConnection()
	d.log.Debug("fetched", "server", name, "msgid", req.messageID, "bytes", len(body))

	// Durability (B.6): the article is not marked Done here. The assembler
	// calls MarkArticleDone after pwrite + fsync so that Done => bytes on disk.
	//
	// MarkArticleEmitted (transient, not persisted) keeps the dispatcher
	// from re-picking this article between now and the assembler's Done
	// write. If the process crashes before fsync, Emitted is lost on
	// restart, so the article is re-dispatched — matching the B.6
	// invariant that Done means "bytes on stable storage".
	if err := d.queue.MarkArticleEmitted(req.jobID, req.messageID); err != nil {
		d.log.Warn("mark article emitted failed", "job", req.jobID, "msgid", req.messageID, "err", err)
	}
	d.emitResult(ctx, req, name, body, nil)
}

// emitResult publishes an ArticleResult on the completions channel.
// Blocks until the consumer reads or ctx fires; the signalDispatch
// in handleRequest's defer ensures the dispatcher wakes up regardless
// of outcome.
func (d *Downloader) emitResult(ctx context.Context, req *articleRequest, server string, body []byte, err error) {
	res := &ArticleResult{
		JobID:      req.jobID,
		FileIdx:    req.fileIdx,
		MessageID:  req.messageID,
		Subject:    req.subject,
		ServerName: server,
		Body:       body,
		Err:        err,
	}
	select {
	case d.completions <- res:
	case <-ctx.Done():
	}
}

// clearInFlight decrements the in-flight counter for an article.
// Called from handleRequest's defer, before signalDispatch, so the
// next dispatch pass observes the cleared state and can fan out to
// a fallback server if the try-list allows.
func (d *Downloader) clearInFlight(jobID, messageID string) {
	key := articleKey{jobID: jobID, messageID: messageID}
	d.tryMu.Lock()
	defer d.tryMu.Unlock()
	if d.inFlight[key] <= 1 {
		delete(d.inFlight, key)
		return
	}
	d.inFlight[key]--
}

// unmarkTried removes serverName from an article's try-list, used
// after a retryable failure (dial error, mid-stream disconnect) so
// the dispatcher can hand the article back to the same server once
// it recovers, or bounce it to another.
func (d *Downloader) unmarkTried(jobID, messageID, serverName string) {
	d.tryMu.Lock()
	defer d.tryMu.Unlock()
	key := articleKey{jobID: jobID, messageID: messageID}
	set, ok := d.tryList[key]
	if !ok {
		return
	}
	delete(set, serverName)
	if len(set) == 0 {
		delete(d.tryList, key)
	}
}
