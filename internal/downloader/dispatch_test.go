package downloader

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/queue"
)

// newDispatchDownloader builds a minimal Downloader suitable for white-box
// tryDispatch / buildDispatchPlan tests. No goroutines are started; workCh
// channels are buffered so non-blocking sends in tryDispatch succeed.
func newDispatchDownloader(servers []*Server) *Downloader {
	workCh := make(map[string]chan *articleRequest, len(servers))
	for _, srv := range servers {
		workCh[srv.Cfg().Name] = make(chan *articleRequest, 1)
	}
	d := &Downloader{
		log:          slog.New(slog.DiscardHandler),
		servers:      servers,
		workCh:       workCh,
		tracker:      newDispatchTracker(),
		completions:  make(chan *ArticleResult, 10),
		connActivity: make(map[string]*ConnActivity),
	}
	ch := make(chan struct{})
	d.disconnectPtr.Store(&ch)
	return d
}

// fakeSrv returns a Server with the given name/priority/enabled state.
// No TCP address is required because tryDispatch never dials.
func fakeSrv(name string, priority int, enabled bool) *Server {
	return NewServer(config.ServerConfig{
		Name:        name,
		Priority:    priority,
		Connections: 1,
		Enable:      enabled,
	})
}

// fakeArticle returns an UnfinishedArticle with the given message-ID under jobID "j1".
func fakeArticle(msgID string) queue.UnfinishedArticle {
	return queue.UnfinishedArticle{
		JobID:     "j1",
		JobStatus: constants.StatusDownloading,
		JobAdded:  time.Now().Add(-time.Hour),
		MessageID: msgID,
		FileIdx:   0,
		Bytes:     100,
		Subject:   "test.bin",
	}
}

// defaultOpts builds a dispatchOpts pointing at the given servers with
// conservative defaults (no maxArtTries cap, no topOnly, no prop delay).
func defaultOpts(servers []*Server) dispatchOpts {
	cfgs := make([]config.ServerConfig, len(servers))
	for i, s := range servers {
		cfgs[i] = s.Cfg()
	}
	return dispatchOpts{
		now:        time.Now(),
		serverCfgs: cfgs,
	}
}

// ---------- tryDispatch unit tests ----------

// A fresh article on a single active server: dispatched and workCh receives req.
func TestTryDispatch_SuccessfulSend(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	a := fakeArticle("msg1@h")
	opts := defaultOpts(d.servers)

	handled, exReq := d.tryDispatch(context.Background(), a, opts)

	if !handled {
		t.Fatal("handled: got false, want true")
	}
	if exReq != nil {
		t.Fatalf("exReq: got non-nil (article exhausted), want nil (dispatched)")
	}
	// Verify req landed in the correct server's workCh.
	select {
	case req := <-d.workCh["s1"]:
		if req.messageID != "msg1@h" {
			t.Errorf("req.messageID = %q, want %q", req.messageID, "msg1@h")
		}
	default:
		t.Error("workCh[s1] is empty — req was not sent")
	}
	d.tracker.Lock()
	inFlightVal := d.tracker.InFlightLocked("msg1@h")
	mask, _ := d.tracker.TryListLocked("msg1@h")
	d.tracker.Unlock()

	// inFlight must be incremented so a second dispatch pass skips this article.
	if inFlightVal != 1 {
		t.Errorf("inFlight = %d, want 1", inFlightVal)
	}
	// try-list bit for server 0 must be set.
	if !mask.has(0) {
		t.Error("tryList[msg1@h] bit 0 not set after dispatch")
	}
}

// An article already in-flight: tryDispatch returns (false, nil) immediately.
func TestTryDispatch_AlreadyInFlight(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	d.tracker.Lock()
	d.tracker.IncrementInFlightLocked("msg1@h")
	d.tracker.Unlock()
	a := fakeArticle("msg1@h")
	opts := defaultOpts(d.servers)

	handled, exReq := d.tryDispatch(context.Background(), a, opts)

	if handled {
		t.Error("handled: got true, want false (article already in flight)")
	}
	if exReq != nil {
		t.Error("exReq: got non-nil, want nil")
	}
}

// Cancelled context: tryDispatch bails out before the server loop.
func TestTryDispatch_CancelledContext(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	a := fakeArticle("msg1@h")
	opts := defaultOpts(d.servers)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	handled, exReq := d.tryDispatch(ctx, a, opts)

	if handled {
		t.Error("handled: got true, want false for cancelled context")
	}
	if exReq != nil {
		t.Error("exReq: got non-nil, want nil")
	}
}

// maxArtTries cap: article tried on enough servers → declared exhausted.
func TestTryDispatch_MaxArtTriesExhausted(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	// Mark server 0 as already tried.
	mask := serverMask{}
	mask.set(0)
	d.tracker.Lock()
	d.tracker.SetTriedLocked("msg1@h", mask)
	d.tracker.Unlock()
	a := fakeArticle("msg1@h")
	opts := defaultOpts(d.servers)
	opts.maxArtTries = 1 // cap at 1 try

	handled, exReq := d.tryDispatch(context.Background(), a, opts)

	if !handled {
		t.Error("handled: got false, want true (exhausted)")
	}
	if exReq == nil {
		t.Fatal("exReq: got nil, want non-nil articleRequest for exhausted article")
	}
	if exReq.messageID != "msg1@h" {
		t.Errorf("exReq.messageID = %q, want %q", exReq.messageID, "msg1@h")
	}
}

// All enabled servers have been tried → article exhausted (allTried=true).
func TestTryDispatch_AllServersTriedExhausted(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	// Mark the only server as tried.
	mask := serverMask{}
	mask.set(0)
	d.tracker.Lock()
	d.tracker.SetTriedLocked("msg1@h", mask)
	d.tracker.Unlock()
	a := fakeArticle("msg1@h")
	opts := defaultOpts(d.servers)

	handled, exReq := d.tryDispatch(context.Background(), a, opts)

	if !handled {
		t.Error("handled: got false, want true (all servers tried)")
	}
	if exReq == nil {
		t.Error("exReq: got nil, want non-nil for exhausted article")
	}
}

// Server penalized but untried: allTried stays false → article waits, not exhausted.
func TestTryDispatch_PenalizedServerWaits(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	srv.ApplyPenalty(10 * time.Minute) // server under penalty → Active(now)==false
	d := newDispatchDownloader([]*Server{srv})
	a := fakeArticle("msg1@h")
	opts := defaultOpts(d.servers)

	handled, exReq := d.tryDispatch(context.Background(), a, opts)

	// Article should not be dispatched OR exhausted — it must wait for penalty expiry.
	if handled {
		t.Error("handled: got true, want false (should wait for penalty to expire)")
	}
	if exReq != nil {
		t.Error("exReq: got non-nil, want nil (article must not be exhausted for a penalized server)")
	}
}

// topOnly skips the lower-priority backup server; article is dispatched only to priority-0.
func TestTryDispatch_TopOnlySkipsBackup(t *testing.T) {
	t.Parallel()

	primary := fakeSrv("primary", 0, true)
	backup := fakeSrv("backup", 1, true)
	d := newDispatchDownloader([]*Server{primary, backup})
	a := fakeArticle("msg1@h")
	opts := defaultOpts(d.servers)
	opts.topOnly = true

	handled, exReq := d.tryDispatch(context.Background(), a, opts)

	if !handled {
		t.Error("handled: got false, want true")
	}
	if exReq != nil {
		t.Error("exReq: got non-nil, want nil")
	}
	// req must be in the primary's workCh, not the backup's.
	select {
	case req := <-d.workCh["primary"]:
		if req.messageID != "msg1@h" {
			t.Errorf("primary req.messageID = %q", req.messageID)
		}
	default:
		t.Error("primary workCh empty — req not dispatched to primary")
	}
	select {
	case <-d.workCh["backup"]:
		t.Error("backup workCh received req — topOnly not respected")
	default:
		// correct: backup channel empty
	}
}

// workCh full: tryDispatch must not block; skips to the next server.
func TestTryDispatch_WorkChFullSkipsToNextServer(t *testing.T) {
	t.Parallel()

	s1 := fakeSrv("s1", 0, true)
	s2 := fakeSrv("s2", 0, true)
	d := newDispatchDownloader([]*Server{s1, s2})
	// Fill s1's channel so the send to s1 takes the default branch.
	d.workCh["s1"] <- &articleRequest{messageID: "blocker"}
	a := fakeArticle("msg1@h")
	opts := defaultOpts(d.servers)

	handled, exReq := d.tryDispatch(context.Background(), a, opts)

	if !handled {
		t.Error("handled: got false, want true (should dispatch to s2)")
	}
	if exReq != nil {
		t.Error("exReq: got non-nil, want nil")
	}
	// Drain s1 (still has the blocker) — msg1@h should be in s2.
	<-d.workCh["s1"]
	select {
	case req := <-d.workCh["s2"]:
		if req.messageID != "msg1@h" {
			t.Errorf("s2 req.messageID = %q, want msg1@h", req.messageID)
		}
	default:
		t.Error("s2 workCh empty — req not dispatched when s1 was full")
	}
}

// Disabled server is skipped: with only a disabled server, article is exhausted.
func TestTryDispatch_DisabledServerExhausts(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, false) // disabled
	d := newDispatchDownloader([]*Server{srv})
	a := fakeArticle("msg1@h")
	opts := defaultOpts(d.servers)

	handled, exReq := d.tryDispatch(context.Background(), a, opts)

	// Disabled server is not a candidate. allTried stays true because
	// there are no enabled servers — article is immediately exhausted.
	if !handled {
		t.Error("handled: got false, want true (disabled server = no candidates = exhausted)")
	}
	if exReq == nil {
		t.Error("exReq: got nil, want non-nil for exhausted article")
	}
}

// ---------- buildDispatchPlan unit tests ----------

// Paused jobs are skipped.
func TestBuildDispatchPlan_SkipsPausedJobs(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	d.queue = queue.New()
	job := makeJobWithArticles(t, []string{"p@h"})
	job.Status = constants.StatusPaused
	if err := d.queue.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	opts := defaultOpts(d.servers)

	plan := d.buildDispatchPlan(context.Background(), opts)

	if plan.dispatched != 0 {
		t.Errorf("dispatched = %d, want 0 for paused job", plan.dispatched)
	}
}

// Propagation delay holds back a freshly-added job.
func TestBuildDispatchPlan_PropagationDelayHoldsBack(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	d.queue = queue.New()
	job := makeJobWithArticles(t, []string{"new@h"})
	if err := d.queue.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	opts := defaultOpts(d.servers)
	opts.propagationDelay = 1 * time.Hour // job was added just now; this holds it back

	plan := d.buildDispatchPlan(context.Background(), opts)

	if plan.dispatched != 0 {
		t.Errorf("dispatched = %d, want 0 (prop delay not yet elapsed)", plan.dispatched)
	}
}

// Hopeless job (failedBytes > par2Bytes) lands in hopelessJobs, not dispatched.
func TestBuildDispatchPlan_HopelessJobNotDispatched(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	d.queue = queue.New()
	job := makeJobWithArticles(t, []string{"h@h"})
	// Simulate a hopeless download: more failed bytes than the par2 set can repair.
	job.FailedBytes = 1000
	job.Par2Bytes = 100
	if err := d.queue.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	opts := defaultOpts(d.servers)

	plan := d.buildDispatchPlan(context.Background(), opts)

	if plan.dispatched != 0 {
		t.Errorf("dispatched = %d, want 0 for hopeless job", plan.dispatched)
	}
	if _, ok := plan.hopelessJobs[job.ID]; !ok {
		t.Error("hopelessJobs does not contain the job ID")
	}
}

// Normal job gets dispatched and appears in activeJobs.
func TestBuildDispatchPlan_NormalJobDispatched(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	d.queue = queue.New()
	job := makeJobWithArticles(t, []string{"ok@h"})
	if err := d.queue.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	opts := defaultOpts(d.servers)

	plan := d.buildDispatchPlan(context.Background(), opts)

	if plan.dispatched != 1 {
		t.Errorf("dispatched = %d, want 1", plan.dispatched)
	}
	if _, ok := plan.activeJobs[job.ID]; !ok {
		t.Error("activeJobs does not contain the job ID")
	}
}

func TestBuildDispatchPlan_PropagationDelayZeroDoesNotHoldBackFutureJob(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	d.queue = queue.New()
	job := makeJobWithArticles(t, []string{"new@h"})
	// Set job added time to the future relative to opts.now
	now := time.Now()
	job.Added = now.Add(5 * time.Minute)
	if err := d.queue.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	opts := defaultOpts(d.servers)
	opts.now = now
	opts.propagationDelay = 0 // disabled

	plan := d.buildDispatchPlan(context.Background(), opts)

	if plan.dispatched != 1 {
		t.Errorf("dispatched = %d, want 1 (propagation delay is 0, should not hold back future job)", plan.dispatched)
	}
}

func TestBuildDispatchPlan_DispatchesMultipleArticlesInOnePass(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	d.workCh["s1"] = make(chan *articleRequest, 10)
	d.queue = queue.New()
	job := makeJobWithArticles(t, []string{"msg1@h", "msg2@h", "msg3@h"})
	if err := d.queue.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	opts := defaultOpts(d.servers)

	plan := d.buildDispatchPlan(context.Background(), opts)

	if plan.dispatched != 3 {
		t.Errorf("dispatched = %d, want 3 articles in a single pass", plan.dispatched)
	}
}

func TestBuildDispatchPlan_StopsOnCancelledContext(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	d.queue = queue.New()
	job := makeJobWithArticles(t, []string{"msg1@h", "msg2@h"})
	if err := d.queue.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	opts := defaultOpts(d.servers)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	plan := d.buildDispatchPlan(ctx, opts)

	if plan.dispatched != 0 {
		t.Errorf("dispatched = %d, want 0 on cancelled context", plan.dispatched)
	}
}

func TestApplyDispatchPlan_IdleDisconnect(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	d.queue = queue.New()

	// Set up connActivity to simulate an active connection.
	d.connActivityMu.Lock()
	d.connActivity["s1#0"] = &ConnActivity{
		ServerName: "s1",
		ConnIndex:  0,
		Connected:  true,
	}
	d.connActivityMu.Unlock()

	// Initial disconnect channel.
	ch := d.disconnectSnapshot()

	// Case 1: plan.dispatched > 0. Should NOT disconnect.
	plan := dispatchPlan{
		dispatched: 1,
	}
	d.applyDispatchPlan(context.Background(), plan, dispatchOpts{})
	select {
	case <-ch:
		t.Fatal("disconnect channel was closed when plan.dispatched > 0")
	default:
		// correct
	}

	// Case 2: plan.dispatched == 0, but queue has downloadable jobs. Should NOT disconnect.
	job := makeJobWithArticles(t, []string{"msg1@h"})
	if err := d.queue.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	plan = dispatchPlan{
		dispatched: 0,
	}
	d.applyDispatchPlan(context.Background(), plan, dispatchOpts{})
	select {
	case <-ch:
		t.Fatal("disconnect channel was closed when queue has downloadable jobs")
	default:
		// correct
	}

	// Case 3: plan.dispatched == 0, queue has no downloadable jobs. Should disconnect.
	// Empty the queue.
	d.queue = queue.New()
	plan = dispatchPlan{
		dispatched: 0,
	}
	d.applyDispatchPlan(context.Background(), plan, dispatchOpts{})
	select {
	case <-ch:
		// correct, it closed the channel!
	default:
		t.Fatal("disconnect channel was NOT closed when idle and no downloadable jobs")
	}
}

func TestTryDispatch_TopOnlySkipsBackupWhenPrimaryFull(t *testing.T) {
	t.Parallel()

	primary := fakeSrv("primary", 0, true)
	backup := fakeSrv("backup", 1, true)
	d := newDispatchDownloader([]*Server{primary, backup})
	// Fill primary's channel
	d.workCh["primary"] <- &articleRequest{messageID: "blocker"}
	a := fakeArticle("msg1@h")
	opts := defaultOpts(d.servers)
	opts.topOnly = true

	handled, exReq := d.tryDispatch(context.Background(), a, opts)

	if handled {
		t.Error("handled: got true, want false (primary full, backup should be skipped under topOnly)")
	}
	if exReq != nil {
		t.Error("exReq: got non-nil, want nil")
	}
	// Verify backup channel is empty
	select {
	case <-d.workCh["backup"]:
		t.Error("backup workCh received req — backup was not skipped when primary was full under topOnly")
	default:
		// correct
	}
}

func TestGetMinServerPriority(t *testing.T) {
	t.Parallel()

	// Case 1: no servers
	if got := getMinServerPriority(nil); got != -1 {
		t.Errorf("got %d, want -1", got)
	}

	// Case 2: only disabled servers
	cfgs := []config.ServerConfig{
		{Priority: 0, Enable: false},
		{Priority: 1, Enable: false},
	}
	if got := getMinServerPriority(cfgs); got != -1 {
		t.Errorf("got %d, want -1", got)
	}

	// Case 3: enabled servers with priority >= 2
	cfgs = []config.ServerConfig{
		{Priority: 3, Enable: true},
		{Priority: 2, Enable: true},
	}
	if got := getMinServerPriority(cfgs); got != 2 {
		t.Errorf("got %d, want 2", got)
	}
}

func BenchmarkDownloader_Dispatch(b *testing.B) {
	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	a := fakeArticle("msg1@h")
	opts := defaultOpts(d.servers)
	ctx := context.Background()

	b.ResetTimer()
	for range b.N {
		// Teardown state directly on the maps to avoid any tracker method/lock overhead.
		delete(d.tracker.inFlight, a.MessageID)
		delete(d.tracker.tryList, a.MessageID)
		select {
		case <-d.workCh["s1"]:
		default:
		}
		_, _ = d.tryDispatch(ctx, a, opts)
	}
}

func TestDownloader_ApplyDispatchPlan_SideEffects(t *testing.T) {
	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	q := queue.New()
	d.queue = q

	// 1. Test plan.exhausted side-effects
	job := makeJobWithArticles(t, []string{"msg1@h"})
	if err := q.Add(job); err != nil {
		t.Fatalf("q.Add: %v", err)
	}

	// Setup tried mapping to test that it gets cleared
	d.tracker.Lock()
	mask := serverMask{}
	mask.set(0)
	d.tracker.SetTriedLocked("msg1@h", mask)
	d.tracker.Unlock()

	plan := dispatchPlan{
		exhausted: []*articleRequest{{
			jobID:     job.ID,
			messageID: "msg1@h",
			bytes:     100,
		}},
	}

	d.applyDispatchPlan(context.Background(), plan, dispatchOpts{})

	// Check completions channel received the ErrNoServersLeft result
	select {
	case res := <-d.Completions():
		if res.MessageID != "msg1@h" || !errors.Is(res.Err, ErrNoServersLeft) {
			t.Errorf("unexpected completion result: %+v", res)
		}
	default:
		t.Error("expected completions channel to receive article result")
	}

	// Check tryList was cleared
	tryListLen, _ := d.tracker.Len()
	if tryListLen != 0 {
		t.Error("expected tryList to be cleared for the exhausted article")
	}

	// 2. Test plan.hopelessJobs side-effects (with callback)
	var callbackJob string
	opts := dispatchOpts{
		onJobHopeless: func(jobID string) {
			callbackJob = jobID
		},
	}
	plan = dispatchPlan{
		hopelessJobs: map[string]struct{}{
			job.ID: {},
		},
	}
	d.applyDispatchPlan(context.Background(), plan, opts)
	if callbackJob != job.ID {
		t.Errorf("expected hopeless callback to fire for %s, got %s", job.ID, callbackJob)
	}

	// 3. Test plan.hopelessJobs fallback (without callback -> should pause the job in queue)
	plan = dispatchPlan{
		hopelessJobs: map[string]struct{}{
			job.ID: {},
		},
	}
	d.applyDispatchPlan(context.Background(), plan, dispatchOpts{}) // no callback
	snap := q.SnapshotJob(job.ID)
	if snap == nil || snap.Status != constants.StatusPaused {
		t.Errorf("expected job status to be Paused, got %+v", snap)
	}
}

func TestAllServersFull(t *testing.T) {
	srv1 := fakeSrv("s1", 0, true)
	srv2 := fakeSrv("s2", 0, true)
	d := newDispatchDownloader([]*Server{srv1, srv2})

	opts := defaultOpts(d.servers)

	// Initially, both channels have capacity (len 0, cap 1).
	if d.allServersFull(opts.serverCfgs) {
		t.Error("expected allServersFull=false when channels are empty")
	}

	// Fill s1
	d.workCh["s1"] <- &articleRequest{messageID: "msg1"}
	if d.allServersFull(opts.serverCfgs) {
		t.Error("expected allServersFull=false when s2 still has capacity")
	}

	// Fill s2
	d.workCh["s2"] <- &articleRequest{messageID: "msg2"}
	if !d.allServersFull(opts.serverCfgs) {
		t.Error("expected allServersFull=true when both active enabled channels are full")
	}

	// Drain s2 but penalize/deactivate s2
	<-d.workCh["s2"]
	srv2.ApplyPenalty(time.Hour)
	if !d.allServersFull(opts.serverCfgs) {
		t.Error("expected allServersFull=true when s1 is full and s2 is inactive")
	}

	// Drain s1
	<-d.workCh["s1"]
	if d.allServersFull(opts.serverCfgs) {
		t.Error("expected allServersFull=false when s1 is empty and s2 is inactive")
	}

	// If all servers are inactive or disabled, allServersFull must return false
	srv1.ApplyPenalty(time.Hour)
	if d.allServersFull(opts.serverCfgs) {
		t.Error("expected allServersFull=false when no active servers exist")
	}
}

func TestBuildDispatchPlan_EarlyExitWhenServersFull(t *testing.T) {
	q := queue.New()
	job := makeJobWithArticles(t, []string{"a@h", "b@h", "c@h", "d@h", "e@h", "f@h", "g@h", "h@h", "i@h", "j@h"})
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}

	srv1 := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv1})
	d.queue = q
	opts := defaultOpts(d.servers)

	// Pre-fill the single capacity-1 work channel for s1 so that allServersFull is true immediately
	d.workCh["s1"] <- &articleRequest{messageID: "prefill"}

	plan := d.buildDispatchPlan(context.Background(), opts)
	if plan.dispatched != 0 {
		t.Errorf("dispatched = %d, want 0 when server is already full", plan.dispatched)
	}

	if !d.allServersFull(opts.serverCfgs) {
		t.Error("expected allServersFull=true")
	}
}
