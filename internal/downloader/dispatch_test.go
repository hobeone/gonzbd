package downloader

import (
	"context"
	"io"
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
	return &Downloader{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		servers:  servers,
		workCh:   workCh,
		tryList:  make(map[string]serverMask),
		inFlight: make(map[string]int),
	}
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
	// inFlight must be incremented so a second dispatch pass skips this article.
	if d.inFlight["msg1@h"] != 1 {
		t.Errorf("inFlight = %d, want 1", d.inFlight["msg1@h"])
	}
	// try-list bit for server 0 must be set.
	mask := d.tryList["msg1@h"]
	if !mask.has(0) {
		t.Error("tryList[msg1@h] bit 0 not set after dispatch")
	}
}

// An article already in-flight: tryDispatch returns (false, nil) immediately.
func TestTryDispatch_AlreadyInFlight(t *testing.T) {
	t.Parallel()

	srv := fakeSrv("s1", 0, true)
	d := newDispatchDownloader([]*Server{srv})
	d.inFlight["msg1@h"] = 1 // simulate an outstanding request
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
	d.tryList["msg1@h"] = mask
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
	d.tryList["msg1@h"] = mask
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
