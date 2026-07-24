package downloader

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/bpsmeter"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/nzb"
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
	// Simulate a hopeless download: more failed bytes than the par2 set can
	// repair. h@h (1000 bytes) is marked failed below; the 100-byte par2
	// file gives Par2Bytes=100 from real NZB classification.
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "test.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "h@h", Bytes: 1000, Number: 1}}},
		{Subject: "test.par2", Bytes: 100, Articles: []nzb.Article{{ID: "idx@h", Bytes: 100, Number: 1}}},
	}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "test.nzb", Priority: constants.NormalPriority}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := d.queue.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	if _, err := d.queue.MarkArticlesFailed(job.ID, []string{"h@h"}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
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

	// Test boundary case where serverCfgs has fewer elements than d.servers.
	// This kills the CONDITIONALS_BOUNDARY mutant at dispatch.go:102.
	optsLessCfgs := defaultOpts(d.servers)
	optsLessCfgs.serverCfgs = optsLessCfgs.serverCfgs[:1]
	_ = d.allServersFull(optsLessCfgs.serverCfgs)
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

func TestTryDispatch_MaxArtTriesExhausted_MultipleServers(t *testing.T) {
	t.Parallel()

	srv1 := fakeSrv("s1", 0, true)
	srv2 := fakeSrv("s2", 1, true)
	d := newDispatchDownloader([]*Server{srv1, srv2})

	// Mark server 0 as already tried, but server 1 is not tried.
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
	select {
	case <-d.workCh["s2"]:
		t.Error("server 2 was incorrectly dispatched to despite maxArtTries limit")
	default:
		// correct
	}
}

func TestDownloader_DecodePayload_Coverage(t *testing.T) {
	// 1. yEnc error branch
	// 2. UU error branch
	// 3. invalid DMCA
	_, _, _, err := decodePayload([]byte("invalid body that fails both yenc and uu"))
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestDownloader_ProcessFetchedArticle_Coverage(t *testing.T) {
	q := queue.New()
	d := &Downloader{
		queue:       q,
		completions: make(chan *ArticleResult, 10),
		log:         slog.Default(),
		tracker:     newDispatchTracker(),
	}

	cfg := config.ServerConfig{Name: "test-server"}
	srv := NewServer(cfg)

	req := &articleRequest{
		jobID:     "job1",
		fileIdx:   0,
		messageID: "msg1",
	}

	// 1. Failure path: non-CRC decode error
	body := []byte("invalid data")
	d.processFetchedArticle(t.Context(), srv, req, body)
	select {
	case res := <-d.completions:
		if res.Err == nil {
			t.Error("expected decode error, got nil")
		}
	default:
		t.Error("expected result emitted, got none")
	}

	// 2. Failure path: CRC mismatch error
	yencBadCRC := []byte("=ybegin line=128 size=12 part=1 name=t.txt\r\n=ypart begin=1 end=12\r\ntest content\r\n=yend size=12 part=1 pcrc32=12345678\r\n")
	d.processFetchedArticle(t.Context(), srv, req, yencBadCRC)
	select {
	case res := <-d.completions:
		if !errors.Is(res.Err, decoder.ErrCRCMismatch) {
			t.Errorf("expected CRC mismatch, got %v", res.Err)
		}
	default:
		t.Error("expected result emitted for CRC mismatch, got none")
	}
}

func TestDownloader_FetchArticle_Coverage(t *testing.T) {
	q := queue.New()
	d := &Downloader{
		queue:   q,
		tracker: newDispatchTracker(),
		log:     slog.Default(),
	}

	d.pauseCtx, d.pauseCancel = context.WithCancel(context.Background())
	defer d.pauseCancel()

	cfg := config.ServerConfig{Name: "test-server"}
	srv := NewServer(cfg)

	req := &articleRequest{
		jobID:     "job1",
		fileIdx:   0,
		messageID: "msg1",
	}

	// 1. Global pause check coverage (d.paused.Load() == true)
	d.paused.Store(true)
	body, ok := d.fetchArticle(t.Context(), srv, 0, &managedConn{}, req, "worker1")
	if ok || body != nil {
		t.Error("expected fetchArticle to return nil, false on global pause")
	}

	// 2. Job paused status check coverage (job paused)
	d.paused.Store(false)
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "movie.mkv", Bytes: 300, Articles: []nzb.Article{
			{ID: "msg1", Bytes: 100, Number: 1},
		}},
	}}
	job, _ := queue.NewJob(parsed, queue.AddOptions{Filename: "m.nzb"}, fsutil.SanitizeOptions{})
	job.Status = constants.StatusPaused
	_ = q.Add(job)

	req.jobID = job.ID
	body, ok = d.fetchArticle(t.Context(), srv, 0, &managedConn{}, req, "worker1")
	if ok || body != nil {
		t.Error("expected fetchArticle to return nil, false on paused job")
	}
}

// ---------- OPT-3: no_penalties / max_art_opt / pre_check ----------

// clampPenalty in isolation, bracketing constants.PenaltyShort on both
// sides so the boundary comparison (pen > constants.PenaltyShort) is
// pinned by an input where the two branches would diverge if a
// mutation flipped > to >=.
func TestClampPenalty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		noPenalties bool
		pen         time.Duration
		want        time.Duration
	}{
		{"disabled: long penalty passes through unclamped", false, constants.PenaltyUnknown, constants.PenaltyUnknown},
		{"enabled: below threshold passes through unclamped", true, constants.PenaltyShort - time.Second, constants.PenaltyShort - time.Second},
		{"enabled: above threshold is clamped down", true, constants.PenaltyShort + time.Second, constants.PenaltyShort},
		{"enabled: zero passes through unclamped", true, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := &Downloader{opts: Options{NoPenalties: tt.noPenalties}}
			got := d.clampPenalty(tt.pen)
			if got != tt.want {
				t.Errorf("clampPenalty(%v) with NoPenalties=%v = %v, want %v", tt.pen, tt.noPenalties, got, tt.want)
			}
		})
	}
}

// NoPenalties must clamp every applied penalty to constants.PenaltyShort,
// even when PenaltyFor(err) would otherwise map the dial error to a much
// longer duration (PenaltyUnknown, the catch-all default for an
// unclassified dial error such as "connection refused").
func TestFetchArticle_NoPenaltiesClampsPenaltyDuration(t *testing.T) {
	t.Parallel()

	// Listener we immediately close: every dial attempt against this
	// address fails with a generic (unclassified) error, which
	// PenaltyFor maps to constants.PenaltyUnknown (3m) by default —
	// well above constants.PenaltyShort (1m).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	q := queue.New()
	srv := testServer(t, "dead", addr)
	d := &Downloader{
		queue:   q,
		tracker: newDispatchTracker(),
		log:     slog.New(slog.DiscardHandler),
		opts:    Options{NoPenalties: true},
		limiter: bpsmeter.NewLimiter(0),
	}
	d.pauseCtx, d.pauseCancel = context.WithCancel(context.Background())
	defer d.pauseCancel()

	req := &articleRequest{jobID: "job1", messageID: "msg1"}
	before := time.Now()
	body, ok := d.fetchArticle(t.Context(), srv, 0, &managedConn{}, req, "worker1")
	if ok || body != nil {
		t.Fatalf("expected fetchArticle to fail against a closed listener")
	}

	expiry := srv.PenaltyExpiry()
	got := expiry.Sub(before)
	if got > constants.PenaltyShort+2*time.Second {
		t.Errorf("penalty duration = %v, want <= constants.PenaltyShort (%v) when NoPenalties is set", got, constants.PenaltyShort)
	}
	if got <= 0 {
		t.Errorf("penalty duration = %v, want > 0 (no_penalties still applies the short penalty, it does not disable penalties entirely)", got)
	}
}

// MaxArtOpt caps the number of optional (backup) servers offered as
// candidates for a single article, independent of MaxArtTries. Once the
// cap is reached, further optional servers must be excluded even if they
// individually have not yet been tried — only the required server should
// remain selectable.
func TestSelectServerForArticle_MaxArtOptCapsOptionalServerTries(t *testing.T) {
	t.Parallel()

	optA := NewServer(config.ServerConfig{Name: "optA", Priority: 0, Connections: 1, Enable: true, Optional: true})
	optB := NewServer(config.ServerConfig{Name: "optB", Priority: 0, Connections: 1, Enable: true, Optional: true})
	required := NewServer(config.ServerConfig{Name: "required", Priority: 0, Connections: 1, Enable: true})
	d := newDispatchDownloader([]*Server{optA, optB, required})

	// optA (idx 0) already tried; optB (idx 1) and required (idx 2) are not.
	mask := serverMask{}
	mask.set(0)

	opts := defaultOpts(d.servers)
	opts.maxArtOpt = 1

	srv, idx := d.selectServerForArticle(mask, true, opts)
	if srv == nil {
		t.Fatal("selectServerForArticle returned nil server, want required server selectable")
	}
	if idx != 2 {
		t.Errorf("selected server idx = %d (%s), want 2 (required) — optB should have been excluded once the optional-server cap was reached", idx, srv.Cfg().Name)
	}
}

// TestIsServerCandidate covers isServerCandidate directly: each of its four
// exclusion branches (already-tried, disabled, topOnly priority mismatch,
// maxArtOpt cap on optional servers) plus the happy-path true case.
func TestIsServerCandidate(t *testing.T) {
	t.Parallel()

	enabledRequired := &config.ServerConfig{Enable: true, Priority: 0}
	enabledOptional := &config.ServerConfig{Enable: true, Priority: 0, Optional: true}
	disabled := &config.ServerConfig{Enable: false, Priority: 0}
	lowPriority := &config.ServerConfig{Enable: true, Priority: 5}

	triedMask := serverMask{}
	triedMask.set(0)
	emptyMask := serverMask{}

	cases := []struct {
		name          string
		cfg           *config.ServerConfig
		mask          serverMask
		hasTried      bool
		idx           int
		topOnly       bool
		minPriority   int
		optionalTried int
		maxArtOpt     int
		want          bool
	}{
		{
			name: "eligible required server", cfg: enabledRequired, mask: emptyMask,
			hasTried: false, idx: 0, topOnly: false, minPriority: 0, optionalTried: 0, maxArtOpt: 0,
			want: true,
		},
		{
			name: "already tried is excluded", cfg: enabledRequired, mask: triedMask,
			hasTried: true, idx: 0, topOnly: false, minPriority: 0, optionalTried: 0, maxArtOpt: 0,
			want: false,
		},
		{
			name: "disabled server is excluded", cfg: disabled, mask: emptyMask,
			hasTried: false, idx: 0, topOnly: false, minPriority: 0, optionalTried: 0, maxArtOpt: 0,
			want: false,
		},
		{
			name: "topOnly excludes lower-priority server", cfg: lowPriority, mask: emptyMask,
			hasTried: false, idx: 0, topOnly: true, minPriority: 0, optionalTried: 0, maxArtOpt: 0,
			want: false,
		},
		{
			name: "topOnly allows server at minPriority", cfg: enabledRequired, mask: emptyMask,
			hasTried: false, idx: 0, topOnly: true, minPriority: 0, optionalTried: 0, maxArtOpt: 0,
			want: true,
		},
		{
			name: "maxArtOpt cap excludes optional server once reached", cfg: enabledOptional, mask: emptyMask,
			hasTried: false, idx: 1, topOnly: false, minPriority: 0, optionalTried: 1, maxArtOpt: 1,
			want: false,
		},
		{
			name: "maxArtOpt cap does not affect required servers", cfg: enabledRequired, mask: emptyMask,
			hasTried: false, idx: 1, topOnly: false, minPriority: 0, optionalTried: 1, maxArtOpt: 1,
			want: true,
		},
		{
			name: "maxArtOpt=0 means unlimited optional tries", cfg: enabledOptional, mask: emptyMask,
			hasTried: false, idx: 1, topOnly: false, minPriority: 0, optionalTried: 5, maxArtOpt: 0,
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isServerCandidate(tc.cfg, tc.mask, tc.hasTried, tc.idx, tc.topOnly, tc.minPriority, tc.optionalTried, tc.maxArtOpt)
			if got != tc.want {
				t.Errorf("isServerCandidate(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

// PreCheck issues an NNTP STAT before BODY. When the server reports the
// article missing via STAT, Fetch (BODY) must never be called — the
// same not-found path taken on a Fetch-time ErrNoArticle is followed
// instead (RecordGoodConnection + emitResult wrapping ErrNoArticle).
func TestFetchArticle_PreCheckSkipsFetchOnMissingArticle(t *testing.T) {
	t.Parallel()

	ms := newMockNNTP(t)
	// Deliberately do not add "missing@h" to ms.bodies, so STAT reports
	// 430 (no such article). If Fetch (BODY) were called anyway, the
	// mock's rejections counter would increment.

	q := queue.New()
	srv := testServer(t, "s", ms.addr)
	d := &Downloader{
		queue:       q,
		tracker:     newDispatchTracker(),
		log:         slog.New(slog.DiscardHandler),
		opts:        Options{PreCheck: true},
		completions: make(chan *ArticleResult, 1),
		limiter:     bpsmeter.NewLimiter(0),
	}
	d.pauseCtx, d.pauseCancel = context.WithCancel(context.Background())
	defer d.pauseCancel()

	req := &articleRequest{jobID: "job1", messageID: "missing@h"}
	mc := &managedConn{}
	defer mc.Close(d, "worker1") // avoid leaving the conn open for the mock's idle-timeout to close (slows the test)
	body, ok := d.fetchArticle(t.Context(), srv, 0, mc, req, "worker1")
	if ok || body != nil {
		t.Fatalf("expected fetchArticle to report not-found, got ok=%v body=%v", ok, body)
	}

	if ms.fetches.Load() != 0 || ms.rejections.Load() != 0 {
		t.Errorf("BODY was called (fetches=%d rejections=%d), want 0 — PreCheck should have short-circuited via STAT",
			ms.fetches.Load(), ms.rejections.Load())
	}

	select {
	case res := <-d.completions:
		if !errors.Is(res.Err, nntp.ErrNoArticle) {
			t.Errorf("completion err = %v, want wrapping nntp.ErrNoArticle", res.Err)
		}
	default:
		t.Error("expected a result on completions for the not-found article")
	}

	if srv.goodConnections() != 1 {
		t.Errorf("goodConnections = %d, want 1 (STAT-based not-found is not a server fault)", srv.goodConnections())
	}
}

// A connection-level failure during Fetch (mid-stream disconnect, distinct
// from a clean 430 ErrNoArticle) must apply the clamped penalty via the
// second clampPenalty call site in fetchArticle. Exercises the "fetch
// failed" branch that TestDownloaderDialFailure and
// TestFetchArticle_PreCheckSkipsFetchOnMissingArticle do not reach.
func TestFetchArticle_FetchFailureAppliesClampedPenalty(t *testing.T) {
	t.Parallel()

	ms := newMockNNTP(t)
	ms.addArticle("hangup@h", "body")
	ms.hangupOnFetch("hangup@h")

	q := queue.New()
	srv := testServer(t, "s", ms.addr)
	d := &Downloader{
		queue:   q,
		tracker: newDispatchTracker(),
		log:     slog.New(slog.DiscardHandler),
		opts:    Options{NoPenalties: true},
		limiter: bpsmeter.NewLimiter(0),
	}
	d.pauseCtx, d.pauseCancel = context.WithCancel(context.Background())
	defer d.pauseCancel()

	req := &articleRequest{jobID: "job1", messageID: "hangup@h"}
	mc := &managedConn{}
	defer mc.Close(d, "worker1")
	before := time.Now()
	body, ok := d.fetchArticle(t.Context(), srv, 0, mc, req, "worker1")
	if ok || body != nil {
		t.Fatalf("expected fetchArticle to report a connection-level failure, got ok=%v body=%v", ok, body)
	}

	if srv.BadConnections() != 1 {
		t.Errorf("BadConnections = %d, want 1 (mid-stream disconnect is a server fault)", srv.BadConnections())
	}

	expiry := srv.PenaltyExpiry()
	got := expiry.Sub(before)
	if got > constants.PenaltyShort+2*time.Second {
		t.Errorf("penalty duration = %v, want <= constants.PenaltyShort (%v) when NoPenalties is set", got, constants.PenaltyShort)
	}
	if got <= 0 {
		t.Errorf("penalty duration = %v, want > 0 for a connection-level fetch failure", got)
	}
}

// Verify that when multiple concurrent fetch requests share a single managedConn
// and a connection hangup/teardown occurs, BadConnections metric is incremented
// exactly once (by the first notifier) and all in-flight article requests are
// cleanly unmarked for retry.
func TestFetchArticle_ConcurrentTeardown_SingleBadConnMetric(t *testing.T) {
	t.Parallel()

	ms := newMockNNTP(t)
	ms.addArticle("hangup1@h", "body1")
	ms.addArticle("hangup2@h", "body2")
	ms.hangupOnFetch("hangup1@h")
	ms.hangupOnFetch("hangup2@h")

	q := queue.New()
	srv := testServer(t, "s", ms.addr)
	tracker := newDispatchTracker()
	d := &Downloader{
		queue:   q,
		tracker: tracker,
		log:     slog.New(slog.DiscardHandler),
		opts:    Options{NoPenalties: true},
		limiter: bpsmeter.NewLimiter(0),
	}
	d.pauseCtx, d.pauseCancel = context.WithCancel(context.Background())
	defer d.pauseCancel()

	// Mark both articles as tried so we can verify unmarkTried on error.
	var initialMask serverMask
	initialMask.set(0)
	tracker.Lock()
	tracker.SetTriedLocked("hangup1@h", initialMask)
	tracker.SetTriedLocked("hangup2@h", initialMask)
	tracker.Unlock()

	mc := &managedConn{}
	defer mc.Close(d, "worker1")

	req1 := &articleRequest{jobID: "job1", messageID: "hangup1@h"}
	req2 := &articleRequest{jobID: "job1", messageID: "hangup2@h"}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		body, ok := d.fetchArticle(t.Context(), srv, 0, mc, req1, "worker1")
		if ok || body != nil {
			t.Errorf("goroutine 1: expected fetchArticle failure, got ok=%v body=%v", ok, body)
		}
	}()

	go func() {
		defer wg.Done()
		body, ok := d.fetchArticle(t.Context(), srv, 0, mc, req2, "worker1")
		if ok || body != nil {
			t.Errorf("goroutine 2: expected fetchArticle failure, got ok=%v body=%v", ok, body)
		}
	}()

	wg.Wait()

	if srv.BadConnections() != 1 {
		t.Errorf("BadConnections = %d, want 1 under concurrent teardown race", srv.BadConnections())
	}

	tracker.Lock()
	m1, _ := tracker.TryListLocked("hangup1@h")
	m2, _ := tracker.TryListLocked("hangup2@h")
	tracker.Unlock()

	if m1.has(0) {
		t.Errorf("hangup1@h still marked tried for server 0, want unmarked after failure")
	}
	if m2.has(0) {
		t.Errorf("hangup2@h still marked tried for server 0, want unmarked after failure")
	}
}

// ---------- selectWork priority pre-check ----------
// https://github.com/hobeone/gonzbd/issues/182

// selectWork must prioritize already-available work over a disconnect
// signal, even when both are simultaneously ready. Without the
// non-blocking pre-check, Go's uniform-random select would occasionally
// pick workDisconnect over a request already sitting in workCh, causing a
// spurious reconnect. Repeated many times because the race is
// probabilistic — a single pass can pass "by luck" on broken code.
func TestSelectWork_PrioritizesReadyWorkOverDisconnect(t *testing.T) {
	t.Parallel()

	const iterations = 200
	for i := range iterations {
		workCh := make(chan *articleRequest, 1)
		want := &articleRequest{messageID: "msg@h"}
		workCh <- want

		disconnectCh := make(chan struct{})
		close(disconnectCh) // simulate DisconnectAll having just fired

		req, decision := selectWork(t.Context(), disconnectCh, workCh)
		if decision != workReady {
			t.Fatalf("iteration %d: decision = %v, want workReady (request was already available)", i, decision)
		}
		if req != want {
			t.Fatalf("iteration %d: got wrong request", i)
		}
	}
}

func TestSelectWork_Branches(t *testing.T) {
	t.Parallel()

	t.Run("cancelled context with no work or disconnect", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		workCh := make(chan *articleRequest)
		disconnectCh := make(chan struct{})
		_, decision := selectWork(ctx, disconnectCh, workCh)
		if decision != workCancelled {
			t.Errorf("decision = %v, want workCancelled", decision)
		}
	})

	t.Run("disconnect signal with no pending work", func(t *testing.T) {
		t.Parallel()
		workCh := make(chan *articleRequest)
		disconnectCh := make(chan struct{})
		close(disconnectCh)
		_, decision := selectWork(t.Context(), disconnectCh, workCh)
		if decision != workDisconnect {
			t.Errorf("decision = %v, want workDisconnect", decision)
		}
	})

	t.Run("work available, disconnect not yet signaled", func(t *testing.T) {
		t.Parallel()
		want := &articleRequest{messageID: "msg@h"}
		workCh := make(chan *articleRequest, 1)
		workCh <- want
		disconnectCh := make(chan struct{})
		req, decision := selectWork(t.Context(), disconnectCh, workCh)
		if decision != workReady || req != want {
			t.Errorf("got (%v, %v), want (%v, workReady)", req, decision, want)
		}
	})
}
