package downloader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/telemetry"
)

// unfinishedArticle is one not-yet-resolved article discovered by
// forEachUnfinishedArticle, plus the per-job/per-file facts buildDispatchPlan
// needs to decide whether to offer it this pass.
//
// It replaces queue.UnfinishedArticle: this package no longer has a
// queue.Queue to ask, and neither job.Job nor dispatch.Dispatcher owns a
// per-article iterator of its own -- enumerating a job's articles for
// dispatch is this package's concern, so the type and the walk that produces
// it both live here.
type unfinishedArticle struct {
	jobID string
	// jobIntent is job.Job.Intent(), read once per job. Replaces the old
	// a.JobStatus == constants.StatusPaused check: per-job pause is now
	// job.IntentPause (see internal/dispatch/registry.go's PauseJob), not a
	// constants.Status -- constants.Status is write-only (rendered only by
	// job.ToSABnzbd / dispatch.Row.Status) and this package must not branch
	// on it.
	jobIntent job.Intent
	// repairState is job.Job.RepairState(), read once per job. Replaces the
	// old a.RepairState, which the queue package computed via the shared
	// queue.RepairStateFrom helper; job.Job.RepairState() is now that same
	// door, owned by internal/job.
	repairState job.RepairState
	// jobAdded is dispatch.Row.Header.Added, read once per job. Same
	// quantity the old a.JobAdded carried (when the job was added to the
	// queue) -- internal/config/downloads.go documents propagation delay as
	// measured from that moment, not from an NZB's own posting date, so this
	// reads Header.Added rather than Manifest.FileDate. See dispatch.Header's
	// doc comment (internal/dispatch/registry.go) for why job.Job itself
	// carries no such timestamp.
	jobAdded time.Time

	messageID  string
	fileIdx    int
	artIdx     int32
	bytes      int
	subject    string
	partNumber int
}

// forEachUnfinishedArticle walks every registered, resident job's
// not-yet-resolved (not Done, not already Emitted) articles, calling fn for
// each. fn returning false stops the whole walk (across every job), matching
// the early-exit contract queue.Queue.ForEachUnfinishedArticle used to give
// buildDispatchPlan.
//
// A job is skipped entirely if it is not resident: with no manifest there is
// no per-article data to read, and Job.Manifest()/Progress() have nothing to
// derive from. This mirrors the old queue's behaviour, where a job promoted
// out of memory contributed nothing to the walk either.
//
// A job is also skipped unless its current State is job.Fetching: that is
// the only state under which this package's own articles are the work
// still outstanding. On-demand par2 recovery volumes can move from
// FetchIfNeeded to FetchAlways mid-job (undeferRecovery, triggered by a
// permanent article failure -- see internal/job/progress.go), but that
// trigger fires only while a job is still fetching, so restricting the walk
// to Fetching does not miss a volume un-deferred later.
//
// Files whose FetchPolicy is not FetchAlways (on-demand par2 volumes still
// held, or discarded) or which are already Complete are skipped before the
// per-article loop, the same two exclusions JobProgress.sizeFigures applies.
//
// Reads j.Checkpoint().Progress rather than j.Progress(): the latter returns
// a live, unsynchronized pointer -- Job.Progress's own doc comment says
// "contentMu guards which *JobProgress this Job points to, not that
// record's contents" -- and this walk runs concurrently with every
// connWorker's MarkArticleDone/MarkArticleFailed/MarkArticleEmitted call for
// the SAME job, which mutate that record's bitsets under contentMu.
// Checkpoint() takes a clone under contentMu instead (checkpoint.go), which
// is the one exported door that gives a race-free snapshot to read from
// outside any lock -- confirmed by -race: reading through Progress()
// directly here reproduced a genuine data race against
// markEmitted/clearEmitted's bitset writes (job/progress.go) in this
// package's own test suite before this call was changed to Checkpoint().
func (d *Downloader) forEachUnfinishedArticle(fn func(unfinishedArticle) bool) {
	for _, row := range d.jobs.List() {
		j, ok := d.jobs.Job(row.ID)
		if !ok {
			continue
		}
		m, err := j.Manifest()
		if err != nil {
			// Manifest() returns ErrNotResident exactly when Resident() is
			// false (content.go) -- one enforcement point for one predicate
			// rather than a separate !j.Resident() check ahead of this.
			continue
		}
		if j.State().State != job.Fetching {
			continue
		}
		cp := j.Checkpoint()
		p := cp.Progress
		if p.PendingArticles() == 0 {
			// Early-out before the per-file walk below, and before
			// RepairState()'s own contentMu acquisition: a job with nothing
			// pending has nothing this walk could offer regardless of intent
			// or repair state, the same short-circuit the old queue's walk
			// took (see hasDownloadableJobs' matching early-out).
			continue
		}
		intent := j.Intent()
		repairState := j.RepairState()
		for fi := range m.NumFiles() {
			if p.FileComplete(fi) || p.FileFetchPolicy(fi) != job.FetchAlways {
				continue
			}
			lo, hi := m.FileRange(fi)
			subject := m.FileSubject(fi)
			for artIdx := lo; artIdx < hi; artIdx++ {
				if p.ArticleDone(artIdx) || p.ArticleEmitted(artIdx) {
					continue
				}
				if !fn(unfinishedArticle{
					jobID:       row.ID,
					jobIntent:   intent,
					repairState: repairState,
					jobAdded:    row.Header.Added,
					messageID:   m.ArticleID(artIdx),
					fileIdx:     fi,
					artIdx:      int32(artIdx), //nolint:gosec // G115: article counts are far below int32
					bytes:       m.ArticleBytes(artIdx),
					subject:     subject,
					partNumber:  m.ArticleNumber(artIdx),
				}) {
					return
				}
			}
		}
	}
}

// hasDownloadableJobs reports whether any registered, resident, non-paused,
// non-hopeless job still has pending articles. Replaces
// queue.Queue.HasDownloadableJobs for applyDispatchPlan's idle-disconnect
// check.
//
// Only called when buildDispatchPlan early-exited on allServersFull without
// ever reaching forEachUnfinishedArticle (dispatchPlan.serversWereFull) --
// every other pass already answers this question for free while it walks,
// via dispatchPlan.downloadable, so calling this too would clone every
// Fetching job's progress record a second time in the steady state
// (plan.dispatched == 0, servers not full) for no new information.
func (d *Downloader) hasDownloadableJobs() bool {
	for _, row := range d.jobs.List() {
		j, ok := d.jobs.Job(row.ID)
		if !ok || !j.Resident() || j.State().State != job.Fetching {
			continue
		}
		// != IntentRun, not == IntentPause: a cancelled job (IntentCancel)
		// must not count as downloadable either -- see forEachUnfinishedArticle's
		// matching check for why.
		if j.Intent() != job.IntentRun {
			continue
		}
		if j.RepairState().Hopeless() {
			continue
		}
		// j.Checkpoint().Progress, not j.Progress(): see
		// forEachUnfinishedArticle's doc comment for why a raw read through
		// Progress() races concurrent article-accounting writers.
		if j.Checkpoint().Progress.PendingArticles() > 0 {
			return true
		}
	}
	return false
}

// markArticleEmitted flags an article as handed to the downloader, through
// the job's own MarkArticleEmitted door. Tolerates a job that is no longer
// registered or not resident -- mirrors the old
// !errors.Is(err, queue.ErrNotFound) && !errors.Is(err, queue.ErrJobNotResident)
// tolerance at each of this function's three call sites, now expressed as
// JobSource.Job's ok bool plus job.ErrNotResident.
func (d *Downloader) markArticleEmitted(jobID, msgID string, artIdx int32) {
	j, ok := d.jobs.Job(jobID)
	if !ok {
		return
	}
	if err := j.MarkArticleEmitted(int(artIdx)); err != nil && !errors.Is(err, job.ErrNotResident) {
		d.log.Warn("mark article emitted failed", "job", jobID, "msgid", msgID, "err", err)
	}
}

// clearArticleEmitted undoes markArticleEmitted for a work item that was
// never dispatched or whose result was dropped. Same tolerance as
// markArticleEmitted.
func (d *Downloader) clearArticleEmitted(jobID, msgID string, artIdx int32) {
	j, ok := d.jobs.Job(jobID)
	if !ok {
		return
	}
	if err := j.ClearArticleEmitted(int(artIdx)); err != nil && !errors.Is(err, job.ErrNotResident) {
		d.log.Warn("clear article emitted failed", "job", jobID, "msgid", msgID, "err", err)
	}
}

// dispatchPlan holds the results of one iteration over the unfinished
// article queue. buildDispatchPlan populates it; dispatchPass applies it.
// Keeping the two phases separate makes the iteration logic unit-testable
// without needing to drive goroutines or NNTP connections.
type dispatchPlan struct {
	dispatched   int                 // number of articles handed to a server
	hopelessJobs map[string]struct{} // jobs whose job.RepairState is Hopeless()
	exhausted    []*articleRequest   // articles with no eligible server this pass

	// downloadable is true if forEachUnfinishedArticle yielded at least one
	// article whose job is IntentRun and not Hopeless -- i.e. what
	// hasDownloadableJobs would have found, computed for free during the
	// walk buildDispatchPlan already performs, rather than by a second walk
	// and a second Checkpoint() clone per job in applyDispatchPlan. Only
	// meaningful when serversWereFull is false; see hasDownloadableJobs'
	// doc comment.
	downloadable bool
	// serversWereFull is true when buildDispatchPlan returned before ever
	// calling forEachUnfinishedArticle (the allServersFull early exit), in
	// which case downloadable was never computed and applyDispatchPlan must
	// fall back to calling hasDownloadableJobs directly.
	serversWereFull bool
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

// buildDispatchPlan iterates over every registered job's unfinished articles
// and populates a dispatchPlan. All side effects (job pausing, result
// emission, idle disconnect) are deferred to the caller's applyDispatchPlan
// call.
func (d *Downloader) buildDispatchPlan(ctx context.Context, opts dispatchOpts) dispatchPlan {
	plan := dispatchPlan{
		hopelessJobs: make(map[string]struct{}),
	}

	if d.allServersFull(opts.serverCfgs) {
		plan.serversWereFull = true
		return plan
	}

	d.forEachUnfinishedArticle(func(a unfinishedArticle) bool {
		// != IntentRun, not == IntentPause: a cancelled job must stop being
		// offered too. sched latches IntentCancel and settles the job, but it
		// stays Fetching and unsettled until it yields
		// (internal/sched/cancel.go; internal/sched/doc.go documents the
		// transient "Fetching, no lease, IntentCancel" shape), so a cancelled
		// job would otherwise keep being dispatched -- burning bandwidth and
		// provider allowance on a download the user already cancelled -- until
		// that yield lands. Also future-proofs a fourth Intent.
		if a.jobIntent != job.IntentRun {
			return true // skip paused/cancelled jobs, keep iterating
		}

		// Propagation delay: skip jobs that haven't aged enough.
		// Posts need time to propagate to all NNTP servers; dispatching
		// too early causes 430 (article not found) errors on backups.
		if opts.propagationDelay > 0 && opts.now.Before(a.jobAdded.Add(opts.propagationDelay)) {
			return true // too young, skip for now
		}

		// Early Health Gate: stop dispatching a job that cannot be repaired.
		//
		// The verdict comes from job.Job.RepairState(), shared with
		// failMsgForJob and the queue listing. Hopeless() is deliberately
		// false for two non-verdicts: damage within the recognized capacity
		// (bytes cannot prove repairability, only rule it out) and capacity
		// that is unmeasured rather than absent. Killing a download on either
		// is worse than letting post-processing read the real par2 packets.
		if a.repairState.Hopeless() {
			plan.hopelessJobs[a.jobID] = struct{}{}
			return true
		}

		// This article's job is IntentRun and not Hopeless -- exactly what
		// hasDownloadableJobs looks for -- so record it here rather than
		// re-deriving the same answer with a second walk in applyDispatchPlan.
		plan.downloadable = true

		handled, exReq := d.tryDispatch(ctx, a, opts)
		if handled {
			plan.dispatched++
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
// drains exhausted articles, pauses hopeless jobs, and triggers idle
// disconnect. It no longer transitions a job status on dispatch -- there is
// nothing left for this package to own there: a job only appears in
// forEachUnfinishedArticle's walk once its State is already Fetching (set by
// whatever drove it there before this pass ran; State transitions are
// job.Job's own doors, per Standing Design Rule 2), so this package has
// nothing to set.
func (d *Downloader) applyDispatchPlan(ctx context.Context, plan dispatchPlan, opts dispatchOpts) {
	for _, req := range plan.exhausted {
		// Mark Emitted before emitting so a concurrent dispatch pass
		// triggered by another worker's signalDispatch doesn't re-see
		// the article as dispatchable (all try-list entries would still
		// be present, so it would keep re-emitting ErrNoServersLeft in
		// a tight loop until the pipeline finally recorded it Failed
		// through the job's own MarkArticleFailed).
		d.markArticleEmitted(req.jobID, req.messageID, req.artIdx)
		d.clearTried(req.jobID, req.artIdx)
		telemetry.PipelineErrors.Add(telemetry.ErrClassExhaustedAllServers, 1)
		d.emitResult(ctx, req, "", nil, 0, 0, ErrNoServersLeft)
	}

	// Handle hopeless jobs.
	for jobID := range plan.hopelessJobs {
		d.log.Warn("job beyond repair (failed bytes > par2 recovery bytes), marking FAILED", "job", jobID)
		if opts.onJobHopeless != nil {
			opts.onJobHopeless(jobID)
		} else {
			// No PauseJob fallback: job.Intent is "what a PERSON has asked of
			// this job" (internal/job/intent.go), and a hopeless verdict is a
			// machine judgement, not a person's request. Writing IntentPause
			// here would show the job as user-paused in the UI and let a
			// Resume re-arm a download already declared beyond repair --
			// recording a machine verdict on the user-intent axis. This path
			// is cold in production (onJobHopeless is always wired by the
			// real caller); an honest log is what's left to do without it.
			d.log.Warn("job hopeless but no onJobHopeless callback wired; "+
				"job will continue to be walked and re-evaluated every pass",
				"job", jobID)
		}
	}

	// Idle disconnect: if nothing was dispatched and no downloadable
	// work remains, close all NNTP connections. This catches the
	// scenario where in-flight articles for a hopeless/completed job
	// finish after DisconnectAll was already called (those workers
	// missed the earlier signal because they were busy).
	//
	// plan.downloadable is what buildDispatchPlan already learned while
	// walking, at no extra cost; hasDownloadableJobs is called only when
	// that walk never ran (serversWereFull), which is the one case
	// downloadable was never computed for -- see both doc comments.
	downloadable := plan.downloadable
	if plan.serversWereFull {
		downloadable = d.hasDownloadableJobs()
	}
	if plan.dispatched == 0 && !downloadable && d.hasActiveConnections() {
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
// a future signalDispatch (worker completion) or the periodic ticker
// (run's doc comment) will trigger another pass.
func (d *Downloader) dispatchPass(ctx context.Context) {
	if d.paused.Load() || d.jobs.Paused() {
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
//     consumer needs whatever lock dispatchPass is currently holding via
//     forEachUnfinishedArticle (job.Job's contentMu, taken per job by
//     Job.Progress()/Manifest() rather than one lock spanning the whole
//     walk).
//
// A future dispatchReady signal from any worker will bring us back to
// re-try articles that returned (false, nil).
func (d *Downloader) tryDispatch(ctx context.Context, a unfinishedArticle, opts dispatchOpts) (bool, *articleRequest) {
	key := articleKey{jobID: a.jobID, artIdx: a.artIdx}

	d.tracker.Lock()

	if d.tracker.InFlightLocked(key) > 0 {
		d.tracker.Unlock()
		return false, nil
	}

	// Allocate the request only after confirming the article is not
	// already in flight — avoids heap churn for the common skip path.
	req := &articleRequest{
		jobID:     a.jobID,
		messageID: a.messageID,
		fileIdx:   a.fileIdx,
		artIdx:    a.artIdx,
		bytes:     a.bytes,
		subject:   a.subject,

		partNumber: a.partNumber,
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
		d.log.Warn("article exceeded max retries", "msgid", a.messageID, "job", a.jobID, "tries", tries, "max", opts.maxArtTries)
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
		d.log.Warn("article failed on all servers", "msgid", a.messageID, "job", a.jobID)
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
			//
			// sem holds this iteration's slot plus one per still-running
			// handleRequest goroutine, so len(sem) > 1 means a request that
			// could dial (and thus open mc) is still in flight.
			req, decision := selectWork(ctx, d.disconnectChanFor(mc, len(sem) > 1), workCh)
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
func (d *Downloader) handleRequest(ctx context.Context, srv *Server, serverIdx int, mc *managedConn, req *articleRequest, workerID string) {
	d.setConnActivity(workerID, req)
	defer d.clearConnActivity(workerID)
	defer d.signalDispatch()
	defer d.clearInFlight(req.jobID, req.artIdx)

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
	// the user clicked pause (or cancel) on this specific job. Check now
	// before starting any network I/O. j.Intent(), not a constants.Status:
	// per-job pause/cancel is job.Intent (PauseJob/sched's cancel latch), and
	// constants.Status is write-only. != IntentRun rather than ==
	// IntentPause for the same reason as forEachUnfinishedArticle's check:
	// a cancelled job must drop its in-flight article too.
	if j, ok := d.jobs.Job(req.jobID); ok && j.Intent() != job.IntentRun {
		d.unmarkTried(req.jobID, req.artIdx, serverIdx)
		d.clearArticleEmitted(req.jobID, req.messageID, req.artIdx)
		return nil, false
	}

	// Global pause check: same as above but for app-wide pause.
	// The pauseCtx cancellation aborts in-flight reads, but articles
	// sitting in the workCh buffer still need to be drained.
	if d.paused.Load() || d.jobs.Paused() {
		d.unmarkTried(req.jobID, req.artIdx, serverIdx)
		d.clearArticleEmitted(req.jobID, req.messageID, req.artIdx)
		return nil, false
	}

	c, err := mc.Get(fetchCtx, d, srv, workerID)
	if err != nil {
		d.unmarkTried(req.jobID, req.artIdx, serverIdx)
		if errors.Is(err, errServerPenalized) {
			// Don't emit a result — the article is returned to the
			// dispatch pool silently. The deferred signalDispatch
			// triggers a new pass where it can be tried on another
			// server or wait for this server's penalty to expire.
			return nil, false
		}
		if fetchCtx.Err() != nil {
			// Context cancelled = shutdown or pause. Silently return.
			//
			// Nothing needs clearing here, and the reason is worth stating
			// because the old comment claimed the opposite: this article has
			// no Emitted bit set. All three markArticleEmitted call sites run
			// at RESULT-emission time (`grep -n 'd\.markArticleEmitted(' internal/downloader/dispatch.go`
			// finds lines 368, 940, 958), after the fetch — what keeps the
			// dispatcher off an article mid-fetch is the
			// in-flight tracker (tryDispatch's InFlightLocked guard), and
			// handleRequest's deferred clearInFlight releases it.
			//
			// So the next dispatch pass re-offers this article on its own. It
			// does not wait for ClearAllEmitted, and it is unaffected by
			// whether its job was withheld from one (#417).
			return nil, false
		}
		d.log.Warn("dial failed", "server", name, "error", err)
		srv.RecordBadConnection()
		telemetry.PipelineErrors.Add(classifyConnError(err), 1)
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
				telemetry.PipelineErrors.Add(telemetry.ErrClassNNTPNoArticle, 1)
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
			telemetry.PipelineErrors.Add(telemetry.ErrClassNNTPNoArticle, 1)
			d.emitResult(ctx, req, name, nil, 0, 0, err)
			return nil, false
		}
		// Connection-level failure: tear down, re-dial later.
		d.log.Warn("fetch failed", "server", name, "msgid", req.messageID, "error", err)

		isFirstNotifier := mc.DropIfMatches(c, d, workerID)

		// Context cancellation means shutdown or pause — not a server fault.
		// Don't record bad connections, apply penalties, or count this
		// as a diagnostic error; it's expected noise during shutdown/pause.
		if ctx.Err() == nil && fetchCtx.Err() == nil {
			telemetry.PipelineErrors.Add(classifyConnError(err), 1)
			if isFirstNotifier {
				srv.RecordBadConnection()
				if pen := d.clampPenalty(PenaltyFor(err)); pen > 0 {
					d.log.Info("penalty applied", "server", name, "duration", pen)
					srv.ApplyPenalty(pen)
				}
			}
		}
		d.unmarkTried(req.jobID, req.artIdx, serverIdx)
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
	payload, err := decodePayload(body)
	if err != nil {
		if payload.data != nil {
			decoder.PutBuffer(payload.data)
		}
		if errors.Is(err, decoder.ErrCRCMismatch) {
			// CRC mismatch is retryable: the server delivered data but
			// it was corrupted. Keep this server in the try-list (don't
			// unmark — we got bad data from it) and let the dispatcher
			// try another server. The connection is healthy, though.
			d.log.Warn("CRC mismatch, will try alternate server",
				"server", name, "msgid", req.messageID)
			srv.RecordGoodConnection()
			telemetry.PipelineErrors.Add(telemetry.ErrClassCRCMismatch, 1)
			d.emitResult(ctx, req, name, nil, 0, 0, err)
			return
		}
		d.log.Warn("decode error", "msgid", req.messageID, "err", err)
		// Non-CRC decode errors are terminal failures — mark Emitted so
		// the dispatcher never re-picks this article, then clear the tryList.
		d.markArticleEmitted(req.jobID, req.messageID, req.artIdx)
		d.clearTried(req.jobID, req.artIdx)
		telemetry.PipelineErrors.Add(classifyDecodeError(err), 1)
		d.emitResult(ctx, req, name, nil, 0, 0, err)
		return
	}

	// The article is not marked Done here, and this package does not mark it
	// Done at all: only a barrier that has drained and fsynced the file may
	// mint the proof Done requires. This package has no such proof to offer,
	// by design -- see nntp-downloader-contract.md §5 and
	// docs/durability-contract.md.
	//
	// markArticleEmitted (transient, not persisted) keeps the dispatcher from
	// re-picking this article between now and that barrier. If the process
	// crashes first, Emitted is lost on restart, the startup resume sweep
	// cannot prove the bytes, and the article is re-dispatched — which is S3,
	// absence of evidence read as absence.
	d.markArticleEmitted(req.jobID, req.messageID, req.artIdx)
	d.clearTried(req.jobID, req.artIdx)
	d.notePartNumberDisagreement(req, payload.partNumber)
	d.emitResult(ctx, req, name, payload.data, payload.offset, payload.crc, nil)
}

// notePartNumberDisagreement counts the cases where the part ordinal a server
// declared in =ybegin part= is not the NZB segment number that was asked for.
//
// It takes NO action, deliberately. No error class, no try-list change, no
// sentinel in isRetryableDownloaderError — the article is emitted exactly as
// it would have been.
//
// The comparison is novel. Manifest.ArticleNumber had no consumer at all
// before this, and SABnzbd's decoder reads part_begin and part_size without
// ever checking the served part against the segment number, so nothing
// anywhere establishes how often the two agree in the wild. Promoting it
// straight to a correctness decision would risk failing healthy articles
// across every server if some indexer renumbers segments — a fleet-wide
// regression bought with zero evidence. Counting first answers whether the
// case exists at all, at no risk, and #379 records what each outcome means.
//
// Zero on either side disables the comparison: a single-part post declares no
// part=, UU has no equivalent field, and a non-numeric value is not evidence
// of a disagreement.
func (d *Downloader) notePartNumberDisagreement(req *articleRequest, served int) {
	if served == 0 || req.partNumber == 0 || served == req.partNumber {
		return
	}
	telemetry.PartNumberMismatches.Add(1)
	d.log.Warn("served part number disagrees with the NZB segment number; "+
		"counting it, taking no action",
		"job", req.jobID, "msgid", req.messageID,
		"served", served, "expected", req.partNumber)
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
		// The result is dropped, and dropping it falsifies a claim three of
		// this function's callers make just before calling it.
		//
		// markArticleEmitted means "a result for this article is on its
		// way to the pipeline" — it is what stops the dispatcher re-picking
		// the article between emission and the barrier that acks it. Three
		// callers set it and then call this function: applyDispatchPlan's
		// exhausted-try-list path, and processFetchedArticle's
		// terminal-decode-error and ordinary success paths. When the send
		// loses the race to cancellation, no result arrives, so nothing
		// downstream ever clears that bit: forEachUnfinishedArticle skips
		// the article, its file never completes, and only a bulk clear on
		// reload (ClearAllEmitted, internal/job/progress.go) recovers it.
		//
		// That last part is what makes this the owner's job rather than the
		// sweep's. A bulk clear cannot tell this article — abandoned, nothing
		// written — from one whose bytes are on disk awaiting a barrier, so it
		// clears both, and clearing the second re-fetches bytes we already
		// have (#417). Clearing here is precise, and it costs the withheld set
		// nothing.
		//
		// Unconditional because it is a no-op where no bit was set: fetchArticle's
		// two ErrNoArticle paths and processFetchedArticle's CRC-mismatch path
		// reach here without marking.
		d.clearArticleEmitted(req.jobID, req.messageID, req.artIdx)
	}
}

// clearInFlight decrements the in-flight counter for an article.
// Called from handleRequest's defer, before signalDispatch, so the
// next dispatch pass observes the cleared state and can fan out to
// a fallback server if the try-list allows.
func (d *Downloader) clearInFlight(jobID string, artIdx int32) {
	d.tracker.DecrementInFlight(articleKey{jobID: jobID, artIdx: artIdx})
}

// unmarkTried removes serverIdx from an article's try-list, used
// after a retryable failure (dial error, mid-stream disconnect) so
// the dispatcher can hand the article back to the same server once
// it recovers, or bounce it to another.
func (d *Downloader) unmarkTried(jobID string, artIdx int32, serverIdx int) {
	d.tracker.UnmarkTried(articleKey{jobID: jobID, artIdx: artIdx}, serverIdx)
}

// clearTried removes an article's entire try-list entry, freeing
// memory. Called when an article reaches a terminal state (success,
// decode error, or ErrNoServersLeft) and will never be dispatched
// again.
func (d *Downloader) clearTried(jobID string, artIdx int32) {
	d.tracker.ClearTried(articleKey{jobID: jobID, artIdx: artIdx})
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

	// open mirrors "conn != nil" for lock-free reads. Maintained under mu
	// alongside every conn assignment. It exists because mu is held across
	// nntp.Dial (see the doc comment above), so an isOpen() that took mu
	// would block its own connWorker's dispatch select for the whole dial —
	// the parent loop and the handleRequest goroutine that is dialling share
	// this managedConn. Readers tolerate a stale value; see disconnectChanFor.
	open atomic.Bool
}

// isOpen reports whether this slot currently holds a dialled connection.
// Lock-free by design — never take mu here, see the open field.
func (m *managedConn) isOpen() bool { return m.open.Load() }

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
	m.open.Store(true)
	d.setConnConnected(workerID, true)
	return c, nil
}

func (m *managedConn) Close(d *Downloader, workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil {
		_ = m.conn.Close() //nolint:errcheck // discarding a broken conn
		m.conn = nil
		m.open.Store(false)
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
		m.open.Store(false)
		d.setConnConnected(workerID, false)
		return true
	}
	return false
}

// classifyDecodeError maps a terminal (non-CRC-mismatch) decodePayload
// error to a telemetry.PipelineErrors class label. The yEnc/UU
// dual-fallback failure is a joined error (see decodePayload) and isn't
// reliably sub-classifiable further, so it falls into a single
// "decode_failed" bucket alongside any other unrecognized decode error.
func classifyDecodeError(err error) string {
	switch {
	case errors.Is(err, ErrArticleRemoved):
		return telemetry.ErrClassDMCARemoved
	case errors.Is(err, decoder.ErrBodyTooLarge):
		return telemetry.ErrClassDecodeBodyTooLarge
	default:
		return telemetry.ErrClassDecodeFailed
	}
}

// decodePayload decodes an article body using yEnc first, with a
// fallback to UU decoding if the payload is not yEnc encoded.
//
// When neither yEnc nor UU decoding succeeds, the raw body is scanned
// for DMCA/takedown keywords. If found, ErrArticleRemoved is returned
// so the caller does not waste bandwidth retrying on backup servers.
// decodedPayload is what one article body yielded.
//
// A struct rather than a fifth positional return: the fourth was already at
// the limit of what a caller can read without counting, and partNumber is the
// one field a reader is most likely to transpose with offset or crc, both of
// which are also numeric.
type decodedPayload struct {
	data   []byte
	offset int64
	crc    uint32

	// partNumber is the ordinal the SERVER declared in =ybegin part=, which is
	// not necessarily the segment number the NZB asked for. Zero when the
	// article declares none, and for UU, which has no equivalent field.
	partNumber int
}

func decodePayload(body []byte) (decodedPayload, error) {
	article, decErr := decoder.DecodeArticle(body)
	switch {
	case decErr == nil:
		return decodedPayload{
			data:       article.Data,
			offset:     article.Offset,
			crc:        article.CRC,
			partNumber: article.PartNumber,
		}, nil
	case errors.Is(decErr, decoder.ErrNotYEnc):
		if article.Data != nil {
			decoder.PutBuffer(article.Data)
		}
		// Fallback to UU decoding.
		data, _, uuErr := decoder.DecodeUU(body)
		if uuErr == nil {
			// UU carries no offset and no checksum of its own. The CRC is
			// computed here rather than read from the article; the offset is
			// asserted to be 0, which is CORRECT ONLY FOR A SINGLE-PART FILE
			// and is not enforced anywhere.
			//
			// Do not restate this as "single-part by construction". Nothing
			// constructs that: an NZB with two UU segments yields two articles
			// that both claim offset 0, and the belief that UU cannot be
			// multi-part is the reason this went unnoticed.
			//
			// WITHIN ONE OPEN-FILE EPISODE the collision is caught: the
			// assembler resolves one of the two articles permanently failed,
			// so an N-part UU file completes short by N-1 parts and reaches
			// par2 as an ordinary shortfall. Which article loses differs —
			// acceptArticle refuses the ARRIVAL if the incumbent has been
			// reported written, FileWriter.Accept displaces the INCUMBENT if
			// it has not (see offsetSettledBy) — and the accounting is the
			// same either way.
			//
			// ACROSS a restart or a close-handles cycle it is not. FileWriter
			// .acceptedAt is per-open-episode residency by design, so a later
			// segment finds offset 0 unowned and overwrites what is there. The
			// file then completes WRONG rather than short.
			//
			// The durability record DOES report that, but by neither of the
			// two mechanisms a reader would reach for, and an earlier draft
			// of this comment naming one of them was simply wrong. Two runs
			// asserting the SAME offset never abut, so they never merge;
			// mergeAdjacentRuns keeps the longer of the two and drops the
			// other, which is what stops a later FinalizeFile from bounding
			// its truncate to the shorter. The dropped row then contributes
			// nothing to Σ length, so overlapFrom's comparison against the
			// file's stat size (#413) sees no evidence and raises nothing —
			// #413 catches the PARTIAL overlaps, which do leave two rows
			// tiling past the file's end. And §3.5's row count does not
			// withhold the whole-file CRC either, because one row is exactly
			// what survives.
			//
			// What catches it is Commit RETURNING the drop, which the barrier
			// turns into a PostAnomaly naming both articles and the contested
			// offset (durability.Collision). The commit is the last moment the
			// collision exists: afterwards the surviving row is
			// indistinguishable from one that never had a rival. And what
			// withholds the CRC is §3.5's ARTICLE-COVERAGE half — the dropped
			// article's index is in no run's span, so the survivor cannot
			// cover the file's whole article range. Both were added because
			// this shape defeats every check stated in bytes.
			//
			// Either way nothing in the diagnosis names UU as the cause. That
			// is #346, and it is a gap in this offset, not in the collision
			// handling that partly absorbs it.
			//
			// That is not a weaker guarantee than yEnc's. The yEnc trailer's
			// crc32 is a transfer check the decoder has already enforced
			// (ErrCRCMismatch) before the bytes reach this point, and the
			// value returned above is likewise the decoder's own checksum
			// over the decoded output. The fact log uses it to verify OUR
			// bytes on disk after a restart, not to validate the sender, so
			// the format's silence about checksums does not matter — what
			// matters is that every decoded article carries one. Returning 0
			// here made UU articles unverifiable on resume and therefore
			// re-fetched forever.
			return decodedPayload{data: data, crc: crc32.ChecksumIEEE(data)}, nil
		}
		if data != nil {
			decoder.PutBuffer(data)
		}

		// Neither yEnc nor UU. Check for DMCA/takedown notices:
		// removed articles are typically replaced with a plaintext
		// notice by the provider.
		if isDMCA(body) {
			return decodedPayload{}, ErrArticleRemoved
		}
		return decodedPayload{}, fmt.Errorf("yenc: %w; uu fallback: %w", decErr, uuErr)
	default:
		if article.Data != nil {
			decoder.PutBuffer(article.Data)
		}
		return decodedPayload{}, decErr
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
