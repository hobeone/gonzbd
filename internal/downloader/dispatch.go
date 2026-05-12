package downloader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/queue"
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
	// activeJobs collects job IDs that had at least one article dispatched
	// this pass. After the iterator releases the queue RLock, we flip
	// their status from Queued → Downloading so the UI reflects progress.
	activeJobs := make(map[string]struct{})
	// exhausted accumulates articles that had no eligible server this
	// pass. Emitting their ErrNoServersLeft results inline would require
	// sending on the completions channel while tryMu and the queue
	// RLock (from ForEachUnfinishedArticle) are both held — a deadlock
	// when the pipeline consumer itself needs the queue write lock.
	// We drain this list after the iterator returns.
	var exhausted []*articleRequest

	// Snapshot server configs once per pass. srv.Cfg() returns a
	// by-value struct copy; calling it per-article per-server was
	// 0.69s in the pprof (lines 185/191). Cache here.
	serverCfgs := make([]config.ServerConfig, len(d.servers))
	for i, srv := range d.servers {
		serverCfgs[i] = srv.Cfg()
	}

	d.queue.ForEachUnfinishedArticle(func(a queue.UnfinishedArticle) bool {
		if a.JobStatus == constants.StatusPaused {
			return true // skip paused jobs, keep iterating
		}

		// Early Health Gate: Check if the job is beyond repair.
		if a.FailedBytes > a.Par2Bytes {
			hopelessJobs[a.JobID] = struct{}{}
			return true // Move to next job
		}

		handled, exReq := d.tryDispatch(ctx, a.JobID, a.FileIdx, a.MessageID, a.Bytes, a.Subject, now, serverCfgs)
		if handled {
			dispatched++
			activeJobs[a.JobID] = struct{}{}
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
		d.clearTried(req.jobID, req.messageID)
		d.emitResult(ctx, req, "", nil, 0, 0, ErrNoServersLeft)
	}

	// Transition Queued → Downloading for jobs that had articles dispatched.
	for jobID := range activeJobs {
		_ = d.queue.SetStatusIf(jobID, constants.StatusDownloading, constants.StatusQueued)
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

	// Idle disconnect: if nothing was dispatched and no downloadable
	// work remains, close all NNTP connections. This catches the
	// scenario where in-flight articles for a hopeless/completed job
	// finish after DisconnectAll was already called (those workers
	// missed the earlier signal because they were busy).
	if dispatched == 0 && !d.queue.HasDownloadableJobs() && d.hasActiveConnections() {
		d.DisconnectAll()
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
func (d *Downloader) tryDispatch(ctx context.Context, jobID string, fileIdx int, messageID string, bytes int, subject string, now time.Time, serverCfgs []config.ServerConfig) (bool, *articleRequest) {
	key := messageID

	d.tryMu.Lock()
	defer d.tryMu.Unlock()

	if d.inFlight[key] > 0 {
		return false, nil
	}

	// Allocate the request only after confirming the article is not
	// already in flight — avoids heap churn for the common skip path.
	req := &articleRequest{
		jobID:     jobID,
		messageID: messageID,
		fileIdx:   fileIdx,
		bytes:     bytes,
		subject:   subject,
	}

	// Check context once before the server loop rather than inside
	// a 3-case select per server. A 2-case select (send/default) is
	// substantially cheaper in runtime.selectgo.
	if ctx.Err() != nil {
		return false, nil
	}

	mask, hasTried := d.tryList[key]
	anyEligible := false
	allTried := true // assume all tried until proven otherwise
	for idx, srv := range d.servers {
		cfg := &serverCfgs[idx]
		if hasTried && mask.has(idx) {
			continue
		}
		// Permanently disabled servers are not candidates — skip them
		// entirely so they don't prevent allTried from becoming true.
		if !cfg.Enable {
			continue
		}
		// This server hasn't been tried yet.
		if !srv.Active(now) {
			// Server is penalized/inactive but not yet tried.
			// Don't declare the article permanently failed — the
			// penalty will expire and we'll get another chance.
			allTried = false
			continue
		}
		anyEligible = true
		ch, ok := d.workCh[cfg.Name]
		if !ok {
			continue
		}
		select {
		case ch <- req:
			mask.set(idx)
			d.tryList[key] = mask
			d.inFlight[key]++
			return true, nil
		default:
			// server's queue is full; try next server
		}
	}

	// If we found no eligible servers to even try (all are in the
	// tryList), this article is permanently failed for this session.
	// Return the req so dispatchPass can emit ErrNoServersLeft after
	// locks are released.
	//
	// Crucially: only declare exhausted when all enabled servers have
	// been definitively tried (allTried). If some servers are merely
	// penalized (allTried=false), the article should wait for penalty
	// expiry rather than being permanently failed.
	if !anyEligible && allTried {
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
func (d *Downloader) connWorker(ctx context.Context, srv *Server, serverIdx int, workerID string) {
	mc := &managedConn{}
	var workerWg sync.WaitGroup

	defer func() {
		workerWg.Wait()
		d.log.Info("disconnected from server", "server", srv.Cfg().Name, "worker", workerID, "reason", "shutdown")
		mc.Close(d, workerID)
	}()

	name := srv.Cfg().Name
	workCh := d.workCh[name]

	d.log.Info("Creating server connection", "server", srv.Cfg().Host)

	pipelineDepth := max(srv.Cfg().PipeliningRequests, 1)
	// Bound outstanding goroutines per connection to prevent one fast
	// connWorker from eagerly draining the entire workCh. We size
	// the limit to pipelineDepth*2 to allow decode overlap: up to
	// pipelineDepth requests can be on the wire (bounded by
	// nntp.Conn.sem), while another pipelineDepth can be decoding.
	maxOutstanding := pipelineDepth * 2
	sem := make(chan struct{}, maxOutstanding)

	for {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
			// We have capacity — now wait for work or disconnect signal.
			disconnectCh := d.disconnectSnapshot()
			select {
			case <-ctx.Done():
				return
			case <-disconnectCh:
				// DisconnectAll was called — close idle connection.
				// Release our semaphore slot and wait for any in-flight
				// handleRequest goroutines to finish before closing.
				<-sem
				workerWg.Wait()
				d.log.Info("disconnected from server", "server", name, "worker", workerID, "reason", "idle")
				mc.Close(d, workerID)
				// Loop back to wait for new work; will re-dial lazily.
			case req := <-workCh:
				workerWg.Add(1)
				go func(req *articleRequest) {
					defer workerWg.Done()
					defer func() { <-sem }()
					d.handleRequest(ctx, srv, serverIdx, mc, req, workerID)
				}(req)
			}
		}
	}
}

// handleRequest is the per-article workhorse. It owns the
// bookkeeping for try-lists, penalty application, and success/error
// emission. The *nntp.Conn pointer is passed by reference so the
// function can replace it with nil on connection-level failure
// (forcing a re-dial on the next call).
func (d *Downloader) handleRequest(ctx context.Context, srv *Server, serverIdx int, mc *managedConn, req *articleRequest, workerID string) {
	d.setConnActivity(workerID, req)
	defer d.clearConnActivity(workerID)
	defer d.signalDispatch()
	defer d.clearInFlight(req.jobID, req.messageID)

	// Snapshot the current pause context. If Pause() is called while
	// we're mid-Fetch, this context gets cancelled immediately,
	// aborting the TCP read.
	d.pauseMu.RLock()
	fetchCtx := d.pauseCtx
	d.pauseMu.RUnlock()

	name := srv.Cfg().Name

	// Per-job pause check: the article was queued into workCh before
	// the user clicked pause on this specific job. Check now before
	// starting any network I/O.
	if status, err := d.queue.GetJobStatus(req.jobID); err == nil && status == constants.StatusPaused {
		d.unmarkTried(req.jobID, req.messageID, serverIdx)
		_ = d.queue.ClearArticleEmitted(req.jobID, req.messageID)
		return
	}

	// Global pause check: same as above but for app-wide pause.
	// The pauseCtx cancellation aborts in-flight reads, but articles
	// sitting in the workCh buffer still need to be drained.
	if d.paused.Load() || d.queue.IsPaused() {
		d.unmarkTried(req.jobID, req.messageID, serverIdx)
		_ = d.queue.ClearArticleEmitted(req.jobID, req.messageID)
		return
	}

	c, err := mc.Get(fetchCtx, d, srv, workerID)
	if err != nil {
		d.unmarkTried(req.jobID, req.messageID, serverIdx)
		if errors.Is(err, errServerPenalized) {
			// Don't emit a result — the article is returned to the
			// dispatch pool silently. The deferred signalDispatch
			// triggers a new pass where it can be tried on another
			// server or wait for this server's penalty to expire.
			return
		}
		if fetchCtx.Err() != nil {
			// Context cancelled = shutdown or pause. Silently
			// return; the article will be re-dispatched after
			// the reload/unpause via ClearAllEmitted.
			return
		}
		d.log.Warn("dial failed", "server", name, "error", err)
		srv.RecordBadConnection()
		if pen := PenaltyFor(err); pen > 0 {
			d.log.Info("penalty applied", "server", name, "duration", pen)
			srv.ApplyPenalty(pen)
		}
		return
	}

	body, err := c.Fetch(fetchCtx, req.messageID)
	if err != nil {
		if errors.Is(err, nntp.ErrNoArticle) {
			d.log.Debug("article not found", "server", name, "msgid", req.messageID)
			// The server definitively said no. Try-list entry is
			// retained so we won't retry here; connection is
			// healthy — reuse it.
			srv.RecordGoodConnection()
			d.emitResult(ctx, req, name, nil, 0, 0, err)
			return
		}
		// Connection-level failure: tear down, re-dial later.
		d.log.Warn("fetch failed", "server", name, "msgid", req.messageID, "error", err)

		isFirstNotifier := mc.DropIfMatches(c, d, workerID)

		// Context cancellation means shutdown or pause — not a server fault.
		// Don't record bad connections or apply penalties.
		if ctx.Err() == nil && fetchCtx.Err() == nil && isFirstNotifier {
			srv.RecordBadConnection()
			if pen := PenaltyFor(err); pen > 0 {
				d.log.Info("penalty applied", "server", name, "duration", pen)
				srv.ApplyPenalty(pen)
			}
		}
		d.unmarkTried(req.jobID, req.messageID, serverIdx)
		// Don't emit a result for connection-level failures. The
		// article is returned to the dispatch pool via unmarkTried;
		// the deferred signalDispatch triggers retry on another
		// server or this one after penalty expiry. Emitting would
		// risk the pipeline misclassifying the error and inflating
		// FailedBytes for what is a transient server issue.
		return
	}

	srv.RecordGoodConnection()
	d.log.Debug("fetched", "server", name, "msgid", req.messageID, "bytes", len(body))

	// Decoding (Step 3: Parallelize Decoding): Decode article payload
	// directly in the connection goroutine to utilize all CPU cores.
	decodedData, offset, partCRC, err := decodePayload(body)
	if err != nil {
		if errors.Is(err, decoder.ErrCRCMismatch) {
			// CRC mismatch is retryable: the server delivered data but
			// it was corrupted. Keep this server in the try-list (don't
			// unmark — we got bad data from it) and let the dispatcher
			// try another server. The connection is healthy, though.
			d.log.Warn("CRC mismatch, will try alternate server",
				"server", name, "msgid", req.messageID)
			srv.RecordGoodConnection()
			d.emitResult(ctx, req, name, nil, 0, 0, err)
			return
		}
		d.log.Warn("decode error", "msgid", req.messageID, "err", err)
		// Non-CRC decode errors are terminal failures — mark Emitted so
		// the dispatcher never re-picks this article, then clear the tryList.
		if markErr := d.queue.MarkArticleEmitted(req.jobID, req.messageID); markErr != nil {
			d.log.Warn("mark article emitted failed", "job", req.jobID, "msgid", req.messageID, "err", markErr)
		}
		d.clearTried(req.jobID, req.messageID)
		d.emitResult(ctx, req, name, nil, 0, 0, err)
		return
	}

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
	d.clearTried(req.jobID, req.messageID)
	d.emitResult(ctx, req, name, decodedData, offset, partCRC, nil)
}

// emitResult publishes an ArticleResult on the completions channel.
// Blocks until the consumer reads or ctx fires; the signalDispatch
// in handleRequest's defer ensures the dispatcher wakes up regardless
// of outcome.
func (d *Downloader) emitResult(ctx context.Context, req *articleRequest, server string, data []byte, offset int64, crc uint32, err error) {
	res := &ArticleResult{
		JobID:      req.jobID,
		FileIdx:    req.fileIdx,
		MessageID:  req.messageID,
		Subject:    req.subject,
		ServerName: server,
		Data:       data,
		Offset:     offset,
		CRC:        crc,
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
func (d *Downloader) clearInFlight(jobID, messageID string) { //nolint:unparam // jobID kept for future per-job tracking
	key := messageID
	d.tryMu.Lock()
	defer d.tryMu.Unlock()
	if d.inFlight[key] <= 1 {
		delete(d.inFlight, key)
		return
	}
	d.inFlight[key]--
}

// unmarkTried removes serverIdx from an article's try-list, used
// after a retryable failure (dial error, mid-stream disconnect) so
// the dispatcher can hand the article back to the same server once
// it recovers, or bounce it to another.
func (d *Downloader) unmarkTried(jobID, messageID string, serverIdx int) { //nolint:unparam // jobID kept for future per-job tracking
	d.tryMu.Lock()
	defer d.tryMu.Unlock()
	key := messageID
	mask, ok := d.tryList[key]
	if !ok {
		return
	}
	mask.unset(serverIdx)
	if mask.isEmpty() {
		delete(d.tryList, key)
	} else {
		d.tryList[key] = mask
	}
}

// clearTried removes an article's entire try-list entry, freeing
// memory. Called when an article reaches a terminal state (success,
// decode error, or ErrNoServersLeft) and will never be dispatched
// again.
func (d *Downloader) clearTried(jobID, messageID string) { //nolint:unparam // jobID kept for future per-job tracking
	key := messageID
	d.tryMu.Lock()
	defer d.tryMu.Unlock()
	delete(d.tryList, key)
}

// managedConn encapsulates an NNTP connection and the synchronization
// required to lazily dial, use, and safely tear down the connection.
type managedConn struct {
	mu   sync.Mutex
	conn *nntp.Conn
}

func (m *managedConn) Get(ctx context.Context, d *Downloader, srv *Server, workerID string) (*nntp.Conn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn != nil {
		return m.conn, nil
	}

	name := srv.Cfg().Name
	if !srv.Active(time.Now()) {
		return nil, errServerPenalized
	}

	d.log.Info("dialing server", "server", name, "host", srv.Cfg().Host)
	dialOpts := []nntp.DialOption{
		nntp.WithLimiter(d.limiter),
		nntp.WithLogger(d.log),
	}
	if d.meter != nil {
		dialOpts = append(dialOpts, nntp.WithRecorder(d.meter, name))
	}

	c, err := nntp.Dial(ctx, srv.Cfg(), dialOpts...)
	if err != nil {
		return nil, err
	}

	d.log.Info("connected", "server", name, "ssl", c.SSLInfo())
	m.conn = c
	d.setConnConnected(workerID, true)
	return c, nil
}

func (m *managedConn) Close(d *Downloader, workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil {
		_ = m.conn.Close() //nolint:errcheck // discarding a broken conn
		m.conn = nil
		d.setConnConnected(workerID, false)
	}
}

// DropIfMatches closes the connection if the given connection matches
// the current connection. Returns true if it was dropped by this call.
func (m *managedConn) DropIfMatches(c *nntp.Conn, d *Downloader, workerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil && m.conn == c {
		_ = m.conn.Close() //nolint:errcheck // discarding a broken conn
		m.conn = nil
		d.setConnConnected(workerID, false)
		return true
	}
	return false
}

// decodePayload decodes an article body using yEnc first, with a
// fallback to UU decoding if the payload is not yEnc encoded.
//
// When neither yEnc nor UU decoding succeeds, the raw body is scanned
// for DMCA/takedown keywords. If found, ErrArticleRemoved is returned
// so the caller does not waste bandwidth retrying on backup servers.
func decodePayload(body []byte) (decoded []byte, offset int64, crc uint32, err error) {
	article, decErr := decoder.DecodeArticle(body)
	switch {
	case decErr == nil:
		return article.Data, article.Offset, article.CRC, nil
	case errors.Is(decErr, decoder.ErrNotYEnc):
		// Fallback to UU decoding.
		data, _, uuErr := decoder.DecodeUU(body)
		if uuErr == nil {
			return data, 0, 0, nil // UU encoding usually doesn't have offset info or CRC
		}

		// Neither yEnc nor UU. Check for DMCA/takedown notices:
		// removed articles are typically replaced with a plaintext
		// notice by the provider.
		if isDMCA(body) {
			return nil, 0, 0, ErrArticleRemoved
		}
		return nil, 0, 0, fmt.Errorf("yenc: %w; uu fallback: %w", decErr, uuErr)
	default:
		return nil, 0, 0, decErr
	}
}

// dmcaKeywords are lowercase strings that, when present in a non-header
// line of a fetched article body, indicate the article was removed by the
// provider (DMCA takedown, copyright claim, etc). Matches the Python
// SABnzbd check in decoder.py:123.
var dmcaKeywords = [][]byte{
	[]byte("dmca"),
	[]byte("removed"),
	[]byte("cancel"),
	[]byte("blocked"),
}

// isDMCA returns true when the article body appears to be a
// DMCA/takedown replacement notice. We scan non-header lines
// (those not starting with "x-") for the keywords. Only called
// after yEnc/UU decode has already failed, so the body is likely
// a plaintext notice rather than binary data.
func isDMCA(body []byte) bool {
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		lower := bytes.ToLower(bytes.TrimRight(line, "\r"))
		// Skip NNTP extension headers (X-*).
		if bytes.HasPrefix(lower, []byte("x-")) {
			continue
		}
		for _, kw := range dmcaKeywords {
			if bytes.Contains(lower, kw) {
				return true
			}
		}
	}
	return false
}
