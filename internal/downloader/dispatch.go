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

// dispatchPlan holds the results of one iteration over the unfinished
// article queue. buildDispatchPlan populates it; dispatchPass applies it.
// Keeping the two phases separate makes the iteration logic unit-testable
// without needing to drive goroutines or NNTP connections.
type dispatchPlan struct {
	dispatched   int                 // number of articles handed to a server
	activeJobs   map[string]struct{} // jobs that got at least one article dispatched
	hopelessJobs map[string]struct{} // jobs where failedBytes > par2Bytes
	exhausted    []*articleRequest   // articles with no eligible server this pass
}

// dispatchOpts bundles the per-pass constants shared by buildDispatchPlan and
// tryDispatch. Snapshotting them once in dispatchPass (under optsMu.RLock) and
// passing the struct through eliminates the 11-parameter signature on tryDispatch
// and makes both functions directly unit-testable without goroutines or queues.
type dispatchOpts struct {
	now              time.Time
	serverCfgs       []config.ServerConfig
	maxArtTries      int
	maxArtOpt        int
	topOnly          bool
	propagationDelay time.Duration
	onJobHopeless    func(jobID string)
}

// buildDispatchPlan iterates over the unfinished article queue and populates
// a dispatchPlan. It holds the queue RLock (via ForEachUnfinishedArticle)
// for the duration and must not block on channels or take any write locks.
// All side effects (status transitions, result emission, idle disconnect)
// are deferred to the caller's applyDispatchPlan call.
func (d *Downloader) buildDispatchPlan(ctx context.Context, opts dispatchOpts) dispatchPlan {
	plan := dispatchPlan{
		activeJobs:   make(map[string]struct{}),
		hopelessJobs: make(map[string]struct{}),
	}

	if d.allServersFull(opts.serverCfgs) {
		return plan
	}

	d.queue.ForEachUnfinishedArticle(func(a queue.UnfinishedArticle) bool {
		if a.JobStatus == constants.StatusPaused {
			return true // skip paused jobs, keep iterating
		}

		// Propagation delay: skip jobs that haven't aged enough.
		// Posts need time to propagate to all NNTP servers; dispatching
		// too early causes 430 (article not found) errors on backups.
		if opts.propagationDelay > 0 && opts.now.Before(a.JobAdded.Add(opts.propagationDelay)) {
			return true // too young, skip for now
		}

		// Early Health Gate: Check if the job is beyond repair.
		if a.FailedBytes > a.Par2Bytes {
			plan.hopelessJobs[a.JobID] = struct{}{}
			return true
		}

		handled, exReq := d.tryDispatch(ctx, a, opts)
		if handled {
			plan.dispatched++
			plan.activeJobs[a.JobID] = struct{}{}
		}
		if exReq != nil {
			plan.exhausted = append(plan.exhausted, exReq)
		}
		if d.allServersFull(opts.serverCfgs) {
			return false
		}
		// Always continue — per-article send is non-blocking and
		// we want to fan out as much as will fit this pass.
		return ctx.Err() == nil
	})

	return plan
}

// allServersFull reports whether every active, enabled server has a full work channel.
// If true, further tryDispatch calls during this pass are guaranteed to skip every server,
// allowing buildDispatchPlan to early-exit.
func (d *Downloader) allServersFull(serverCfgs []config.ServerConfig) bool {
	now := time.Now()
	hasActive := false
	for i, srv := range d.servers {
		if i >= len(serverCfgs) {
			break
		}
		cfg := &serverCfgs[i]
		if !cfg.Enable || !srv.Active(now) {
			continue
		}
		ch, ok := d.workCh[cfg.Name]
		if !ok {
			continue
		}
		hasActive = true
		if len(ch) < cap(ch) {
			return false
		}
	}
	return hasActive
}

// applyDispatchPlan executes the side effects deferred from buildDispatchPlan:
// drains exhausted articles (must happen after queue RLock is released to
// avoid deadlock on the completions channel), transitions job statuses,
// handles hopeless jobs, and triggers idle disconnect.
func (d *Downloader) applyDispatchPlan(ctx context.Context, plan dispatchPlan, opts dispatchOpts) {
	// Queue RLock and tryMu are both released by now. Safe to block on
	// completions; the pipeline consumer can take the queue write lock.
	for _, req := range plan.exhausted {
		// Mark Emitted before emitting so a concurrent dispatch pass
		// triggered by another worker's signalDispatch doesn't re-see
		// the article as dispatchable (all try-list entries would still
		// be present, so it would keep re-emitting ErrNoServersLeft in
		// a tight loop until the assembler finally marked it Failed).
		if err := d.queue.MarkArticleEmittedByIdx(req.jobID, req.artIdx); err != nil && !errors.Is(err, queue.ErrNotFound) {
			d.log.Warn("mark article emitted failed", "job", req.jobID, "msgid", req.messageID, "err", err)
		}
		d.clearTried(req.jobID, req.messageID)
		d.emitResult(ctx, req, "", nil, 0, 0, ErrNoServersLeft)
	}

	// Transition Queued → Downloading for jobs that had articles dispatched.
	for jobID := range plan.activeJobs {
		_ = d.queue.SetStatusIf(jobID, constants.StatusDownloading, constants.StatusQueued)
	}

	// Handle hopeless jobs.
	for jobID := range plan.hopelessJobs {
		d.log.Warn("job beyond repair (failed bytes > par2 bytes), marking FAILED", "job", jobID)
		if opts.onJobHopeless != nil {
			opts.onJobHopeless(jobID)
		} else {
			_ = d.queue.Pause(jobID) // Fallback if no callback
		}
	}

	// Idle disconnect: if nothing was dispatched and no downloadable
	// work remains, close all NNTP connections. This catches the
	// scenario where in-flight articles for a hopeless/completed job
	// finish after DisconnectAll was already called (those workers
	// missed the earlier signal because they were busy).
	if plan.dispatched == 0 && !d.queue.HasDownloadableJobs() && d.hasActiveConnections() {
		d.DisconnectAll()
	}
}

// dispatchPass walks the queue once and tries to feed every not-yet-done
// article into an eligible server's work channel.
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
func (d *Downloader) dispatchPass(ctx context.Context) {
	if d.paused.Load() || d.queue.IsPaused() {
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	now := time.Now()

	// Snapshot server configs once per pass. srv.Cfg() returns a
	// by-value struct copy; calling it per-article per-server was
	// 0.69s in the pprof. Cache here before acquiring queue RLock.
	serverCfgs := make([]config.ServerConfig, len(d.servers))
	for i, srv := range d.servers {
		serverCfgs[i] = srv.Cfg()
	}

	// Snapshot mutable dispatch options under RLock and bundle them with
	// the server snapshot. Both land in dispatchOpts so tryDispatch and
	// buildDispatchPlan read from stack-local values with no lock held.
	d.optsMu.RLock()
	opts := dispatchOpts{
		now:              now,
		serverCfgs:       serverCfgs,
		maxArtTries:      d.maxArtTries,
		maxArtOpt:        d.maxArtOpt,
		topOnly:          d.topOnly,
		propagationDelay: d.propagationDelay,
		onJobHopeless:    d.onJobHopeless,
	}
	d.optsMu.RUnlock()

	plan := d.buildDispatchPlan(ctx, opts)
	d.applyDispatchPlan(ctx, plan, opts)
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
func (d *Downloader) tryDispatch(ctx context.Context, a queue.UnfinishedArticle, opts dispatchOpts) (bool, *articleRequest) {
	key := a.MessageID

	d.tracker.Lock()

	if d.tracker.InFlightLocked(key) > 0 {
		d.tracker.Unlock()
		return false, nil
	}

	// Allocate the request only after confirming the article is not
	// already in flight — avoids heap churn for the common skip path.
	req := &articleRequest{
		jobID:     a.JobID,
		messageID: a.MessageID,
		fileIdx:   a.FileIdx,
		artIdx:    a.ArtIdx,
		bytes:     a.Bytes,
		subject:   a.Subject,
	}

	// Check context once before the server loop rather than inside
	// a 3-case select per server. A 2-case select (send/default) is
	// substantially cheaper in runtime.selectgo.
	if ctx.Err() != nil {
		d.tracker.Unlock()
		return false, nil
	}

	mask, hasTried := d.tracker.TryListLocked(key)

	// MaxArtTries: if the article has already been tried on the maximum
	// number of servers, declare it permanently failed. This prevents
	// infinite retry loops on articles that consistently fail on all servers.
	if opts.maxArtTries > 0 && hasTried && mask.count() >= opts.maxArtTries {
		tries := mask.count()
		d.tracker.Unlock()
		// --- No lock held below this line ---
		d.log.Warn("article exceeded max retries", "msgid", a.MessageID, "job", a.JobID, "tries", tries, "max", opts.maxArtTries)
		return true, req
	}

	srv, idx := d.selectServerForArticle(mask, hasTried, opts)
	if srv != nil {
		ch, ok := d.workCh[srv.Cfg().Name]
		if ok {
			select {
			case ch <- req:
				mask.set(idx)
				d.tracker.SetTriedLocked(key, mask)
				d.tracker.IncrementInFlightLocked(key)
				d.tracker.Unlock()
				return true, nil
			default:
				// server queue filled between selectServerForArticle and ch <- req
			}
		}
		d.tracker.Unlock()
		return false, nil
	}
	if idx == -1 {
		d.tracker.Unlock()
		// --- No lock held below this line ---
		d.log.Warn("article failed on all servers", "msgid", a.MessageID, "job", a.JobID)
		return true, req
	}
	d.tracker.Unlock()
	return false, nil
}

// selectServerForArticle scans servers for the first eligible candidate with an available work slot.
// Returns (srv, idx) if a server can accept the article right now.
// Returns (nil, -1) if no eligible server exists and all enabled servers have been definitively tried (exhausted).
// Returns (nil, -2) if no server can accept right now (e.g. queues full or temporarily penalized).
func (d *Downloader) selectServerForArticle(mask serverMask, hasTried bool, opts dispatchOpts) (srv *Server, serverIdx int) {
	var minPriority int
	if opts.topOnly {
		minPriority = getMinServerPriority(opts.serverCfgs)
	}

	// MaxArtOpt: count how many optional servers this article has already
	// been tried on. Computed once per call (not per server) — cheap
	// O(len(serverCfgs)) pass reused by every isServerCandidate check
	// below rather than an O(servers^2) recount.
	optionalTried := 0
	if hasTried {
		for idx, cfg := range opts.serverCfgs {
			if cfg.Optional && mask.has(idx) {
				optionalTried++
			}
		}
	}

	anyEligible := false
	allTried := true // assume all tried until proven otherwise
	for idx, srv := range d.servers {
		cfg := &opts.serverCfgs[idx]
		if !isServerCandidate(cfg, mask, hasTried, idx, opts.topOnly, minPriority, optionalTried, opts.maxArtOpt) {
			continue
		}
		if !srv.Active(opts.now) {
			allTried = false
			continue
		}
		anyEligible = true
		ch, ok := d.workCh[cfg.Name]
		if !ok {
			continue
		}
		if len(ch) < cap(ch) {
			return srv, idx
		}
	}

	if !anyEligible && allTried {
		return nil, -1
	}
	return nil, -2
}

// clampPenalty enforces d.opts.NoPenalties: when set, no server penalty may
// exceed constants.PenaltyShort, regardless of the error class PenaltyFor
// would otherwise map to. Kept as a one-line helper so both call sites
// (dial failure, fetch failure) apply the same rule (OPT-3).
func (d *Downloader) clampPenalty(pen time.Duration) time.Duration {
	if d.opts.NoPenalties && pen > constants.PenaltyShort {
		return constants.PenaltyShort
	}
	return pen
}

func getMinServerPriority(serverCfgs []config.ServerConfig) int {
	minPriority := -1
	for idx := range serverCfgs {
		cfg := &serverCfgs[idx]
		if cfg.Enable {
			if minPriority < 0 || cfg.Priority < minPriority {
				minPriority = cfg.Priority
			}
		}
	}
	return minPriority
}

// isServerCandidate evaluates whether a server should be considered for a dispatch try.
// Checks if the server has already been tried, is disabled, or doesn't meet TopOnly priority.
func isServerCandidate(cfg *config.ServerConfig, mask serverMask, hasTried bool, idx int, topOnly bool, minPriority, optionalTried, maxArtOpt int) bool {
	if hasTried && mask.has(idx) {
		return false
	}
	// Permanently disabled servers are not candidates — skip them entirely
	if !cfg.Enable {
		return false
	}
	// TopOnly: skip servers that are not in the primary group.
	if topOnly && cfg.Priority > minPriority {
		return false
	}
	// MaxArtOpt: once an article has been tried on this many optional
	// (backup) servers, stop offering further optional servers — it may
	// still be offered required servers (OPT-3).
	if cfg.Optional && maxArtOpt > 0 && optionalTried >= maxArtOpt {
		return false
	}
	return true
}

// workDecision reports which event connWorker's inner wait resolved to.
type workDecision int

const (
	workReady workDecision = iota
	workDisconnect
	workCancelled
)

// selectWork waits for the next event a connWorker should react to. Ready
// work (or context cancellation) is given priority over a pending
// disconnect signal via a non-blocking pre-check: DisconnectAll broadcasts
// by closing disconnectCh, which then stays permanently select-ready, so a
// plain 3-way select would let Go's uniform-random case selection
// occasionally pick the stale disconnect over a request that just landed
// on workCh — closing a connection that was about to be reused. See
// https://github.com/hobeone/gonzbd/issues/182. The pre-check narrows this
// window to the (much smaller) gap between the two selects; it cannot
// close it entirely, since new work can still arrive in that gap.
func selectWork(ctx context.Context, disconnectCh <-chan struct{}, workCh <-chan *articleRequest) (*articleRequest, workDecision) {
	select {
	case <-ctx.Done():
		return nil, workCancelled
	case req := <-workCh:
		return req, workReady
	default:
	}
	select {
	case <-ctx.Done():
		return nil, workCancelled
	case <-disconnectCh:
		return nil, workDisconnect
	case req := <-workCh:
		return req, workReady
	}
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
		d.log.Debug("disconnected from server", "server", srv.Cfg().Name, "worker", workerID, "reason", "shutdown")
		mc.Close(d, workerID)
	}()

	name := srv.Cfg().Name
	workCh := d.workCh[name]

	d.log.Debug("connection worker started", "server", srv.Cfg().Name, "worker", workerID)

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
			req, decision := selectWork(ctx, disconnectCh, workCh)
			switch decision {
			case workCancelled:
				return
			case workDisconnect:
				// DisconnectAll was called — close idle connection.
				// Release our semaphore slot and wait for any in-flight
				// handleRequest goroutines to finish before closing.
				<-sem
				workerWg.Wait()
				d.log.Debug("disconnected from server", "server", name, "worker", workerID, "reason", "idle")
				mc.Close(d, workerID)
				// Loop back to wait for new work; will re-dial lazily.
			case workReady:
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

	body, ok := d.fetchArticle(ctx, srv, serverIdx, mc, req, workerID)
	if !ok {
		return
	}

	d.processFetchedArticle(ctx, srv, req, body)
}

// fetchArticle performs network I/O and penalty tracking for a single article request.
// Returns (body, true) if the article payload was successfully fetched.
// Returns (nil, false) if the request was handled via pause/retry/error emission.
func (d *Downloader) fetchArticle(ctx context.Context, srv *Server, serverIdx int, mc *managedConn, req *articleRequest, workerID string) ([]byte, bool) {
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
		return nil, false
	}

	// Global pause check: same as above but for app-wide pause.
	// The pauseCtx cancellation aborts in-flight reads, but articles
	// sitting in the workCh buffer still need to be drained.
	if d.paused.Load() || d.queue.IsPaused() {
		d.unmarkTried(req.jobID, req.messageID, serverIdx)
		_ = d.queue.ClearArticleEmitted(req.jobID, req.messageID)
		return nil, false
	}

	c, err := mc.Get(fetchCtx, d, srv, workerID)
	if err != nil {
		d.unmarkTried(req.jobID, req.messageID, serverIdx)
		if errors.Is(err, errServerPenalized) {
			// Don't emit a result — the article is returned to the
			// dispatch pool silently. The deferred signalDispatch
			// triggers a new pass where it can be tried on another
			// server or wait for this server's penalty to expire.
			return nil, false
		}
		if fetchCtx.Err() != nil {
			// Context cancelled = shutdown or pause. Silently
			// return; the article will be re-dispatched after
			// the reload/unpause via ClearAllEmitted.
			return nil, false
		}
		d.log.Warn("dial failed", "server", name, "error", err)
		srv.RecordBadConnection()
		if pen := d.clampPenalty(PenaltyFor(err)); pen > 0 {
			d.log.Info("penalty applied", "server", name, "duration", pen)
			srv.ApplyPenalty(pen)
		}
		return nil, false
	}

	if d.opts.PreCheck {
		if statErr := c.Stat(fetchCtx, req.messageID); statErr != nil {
			if errors.Is(statErr, nntp.ErrNoArticle) {
				d.log.Debug("article not found (precheck)", "server", name, "msgid", req.messageID)
				srv.RecordGoodConnection()
				d.emitResult(ctx, req, name, nil, 0, 0, statErr)
				return nil, false
			}
			// Any other Stat error (connection-level) is handled exactly
			// like a Fetch failure below — fall through to the normal
			// Fetch call so the existing dial/connection-error handling
			// (penalty, bad-connection recording, re-dial) applies
			// uniformly rather than being duplicated here.
		}
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
			return nil, false
		}
		// Connection-level failure: tear down, re-dial later.
		d.log.Warn("fetch failed", "server", name, "msgid", req.messageID, "error", err)

		isFirstNotifier := mc.DropIfMatches(c, d, workerID)

		// Context cancellation means shutdown or pause — not a server fault.
		// Don't record bad connections or apply penalties.
		if ctx.Err() == nil && fetchCtx.Err() == nil && isFirstNotifier {
			srv.RecordBadConnection()
			if pen := d.clampPenalty(PenaltyFor(err)); pen > 0 {
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
		return nil, false
	}

	srv.RecordGoodConnection()
	d.log.Debug("fetched", "server", name, "msgid", req.messageID, "bytes", len(body))
	return body, true
}

// processFetchedArticle decodes an article payload, verifies CRC, and emits the final queue result.
func (d *Downloader) processFetchedArticle(ctx context.Context, srv *Server, req *articleRequest, body []byte) {
	name := srv.Cfg().Name
	// Decoding (Step 3: Parallelize Decoding): Decode article payload
	// directly in the connection goroutine to utilize all CPU cores.
	decodedData, offset, partCRC, err := decodePayload(body)
	if err != nil {
		if decodedData != nil {
			decoder.PutBuffer(decodedData)
		}
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
		if markErr := d.queue.MarkArticleEmitted(req.jobID, req.messageID); markErr != nil && !errors.Is(markErr, queue.ErrNotFound) {
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
	if err := d.queue.MarkArticleEmittedByIdx(req.jobID, req.artIdx); err != nil && !errors.Is(err, queue.ErrNotFound) {
		d.log.Warn("mark article emitted failed", "job", req.jobID, "msgid", req.messageID, "err", err)
	}
	d.clearTried(req.jobID, req.messageID)
	d.emitResult(ctx, req, name, decodedData, offset, partCRC, nil)
}

func (d *Downloader) emitResult(ctx context.Context, req *articleRequest, server string, data []byte, offset int64, crc uint32, err error) {
	res := &ArticleResult{
		JobID:      req.jobID,
		FileIdx:    req.fileIdx,
		ArtIdx:     req.artIdx,
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
	d.tracker.DecrementInFlight(messageID)
}

// unmarkTried removes serverIdx from an article's try-list, used
// after a retryable failure (dial error, mid-stream disconnect) so
// the dispatcher can hand the article back to the same server once
// it recovers, or bounce it to another.
func (d *Downloader) unmarkTried(jobID, messageID string, serverIdx int) { //nolint:unparam // jobID kept for future per-job tracking
	d.tracker.UnmarkTried(messageID, serverIdx)
}

// clearTried removes an article's entire try-list entry, freeing
// memory. Called when an article reaches a terminal state (success,
// decode error, or ErrNoServersLeft) and will never be dispatched
// again.
func (d *Downloader) clearTried(jobID, messageID string) { //nolint:unparam // jobID kept for future per-job tracking
	d.tracker.ClearTried(messageID)
}

// managedConn encapsulates an NNTP connection and the synchronization
// required to lazily dial, use, and safely tear down the connection.
//
// Get deliberately holds mu across the dial itself (nntp.Dial and its
// surrounding log calls) — an intentional exception to the general "never
// hold a mutex during I/O" rule (see docs/go-standards.md), not an oversight:
// mu here doubles as the dial-coalescing lock for this connection slot.
// Every concurrent caller of Get/Close/DropIfMatches for the SAME
// managedConn genuinely needs this exact dial to resolve before it can do
// anything useful (unlike the historical violations this rule targets,
// where the blocked party had unrelated work to do), and releasing the lock
// mid-dial would let two goroutines dial concurrently, or let Close() run
// while a dial is in flight and silently orphan the connection it
// eventually stores. This is closer to sync.Once's pattern (holding for an
// initializer's duration) than to the anti-pattern the rule targets.
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

	d.log.Debug("dialing", "server", name, "host", srv.Cfg().Host) //lockio: see managedConn doc comment — mu is also the dial-coalescing lock
	dialOpts := []nntp.DialOption{
		nntp.WithLimiter(d.limiter),
		nntp.WithLogger(d.log),
	}
	if d.meter != nil {
		dialOpts = append(dialOpts, nntp.WithRecorder(d.meter, name))
	}

	c, err := nntp.Dial(ctx, srv.Cfg(), dialOpts...) //lockio: see managedConn doc comment — mu is also the dial-coalescing lock
	if err != nil {
		return nil, err
	}

	d.log.Debug("connected", "server", name, "ssl", c.SSLInfo()) //lockio: see managedConn doc comment — mu is also the dial-coalescing lock
	d.ensureDisconnectChan()
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
		if article.Data != nil {
			decoder.PutBuffer(article.Data)
		}
		// Fallback to UU decoding.
		data, _, uuErr := decoder.DecodeUU(body)
		if uuErr == nil {
			return data, 0, 0, nil // UU encoding usually doesn't have offset info or CRC
		}
		if data != nil {
			decoder.PutBuffer(data)
		}

		// Neither yEnc nor UU. Check for DMCA/takedown notices:
		// removed articles are typically replaced with a plaintext
		// notice by the provider.
		if isDMCA(body) {
			return nil, 0, 0, ErrArticleRemoved
		}
		return nil, 0, 0, fmt.Errorf("yenc: %w; uu fallback: %w", decErr, uuErr)
	default:
		if article.Data != nil {
			decoder.PutBuffer(article.Data)
		}
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
