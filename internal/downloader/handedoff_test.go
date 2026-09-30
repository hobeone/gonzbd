package downloader

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/bpsmeter"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestFetchArticle_HandedOffJobIsNotFetched: fetchArticle's per-job check
// (dispatch.go, next to the intent check) must drop an article whose job has
// been handed to post-processing, the same way it already drops one for a
// paused, cancelled, or superseded-instance job. Without this, an article
// already queued into workCh (or pipelined) when enqueuePostProc admits the
// job is fetched over NNTP and then discarded by the assembler's whole-job
// tombstone (docs/post-processing-contract.md "An admitted job is not
// downloaded").
func TestFetchArticle_HandedOffJobIsNotFetched(t *testing.T) {
	t.Parallel()

	ms := newMockNNTP(t)
	ms.addArticle("a@h", string(yencBody("a.bin", []byte("payload"))))

	srv := testServer(t, "s", ms.addr)
	d := &Downloader{
		dispatcher:  newTestDispatcher(t),
		tracker:     newDispatchTracker(),
		log:         slog.New(slog.DiscardHandler),
		completions: make(chan *ArticleResult, 1),
		limiter:     bpsmeter.NewLimiter(0),
	}
	d.pauseCtx, d.pauseCancel = context.WithCancel(context.Background())
	defer d.pauseCancel()

	j, m := makeJobWithArticles(t, []string{"a@h"})
	addTestJob(t, d.dispatcher, j, m)
	d.opts.HandedOff = func(x *job.Job) bool { return x == j }

	req := &articleRequest{job: j, messageID: "a@h"}
	mc := &managedConn{}
	defer mc.Close(d, "worker1")
	body, ok := d.fetchArticle(t.Context(), srv, 0, mc, req, "worker1")
	if ok || body != nil {
		t.Errorf("fetchArticle = (%v, %v), want (nil, false) for a handed-off job", body, ok)
	}
	if got := ms.fetches.Load(); got != 0 {
		t.Errorf("fetchArticle for a handed-off job: ok=%v, BODY fetches=%d, want no fetch", ok, got)
	}
	if j.Progress().ArticleEmitted(int(req.artIdx)) {
		t.Errorf("article %d is still marked emitted after a handed-off job's request was dropped", req.artIdx)
	}
}

// TestBuildDispatchPlan_HandOffDuringTheArticleLoop: Options.HandedOff is
// consulted once per job, before buildDispatchPlan's article loop
// (dispatch.go:88). A hand-off that lands after that gate but before the
// loop finishes is not consulted again inside the loop — enqueuePostProc's
// admission (postProcAdmissions.mu) is not serialised with
// ForEachUnfinishedArticle — so the rest of the job's articles are still
// queued into workCh this pass. That is not itself the bug: fetchArticle's
// per-job check, once it also consults HandedOff, drops every one of those
// late-queued articles before any network I/O, so nothing is actually
// fetched.
func TestBuildDispatchPlan_HandOffDuringTheArticleLoop(t *testing.T) {
	ms := newMockNNTP(t)
	ms.addArticle("msg1@h", string(yencBody("a.bin", []byte("payload1"))))
	ms.addArticle("msg2@h", string(yencBody("a.bin", []byte("payload2"))))
	ms.addArticle("msg3@h", string(yencBody("a.bin", []byte("payload3"))))

	srv := testServer(t, "s1", ms.addr)
	d := newDispatchDownloader([]*Server{srv})
	d.workCh["s1"] = make(chan *articleRequest, 10)
	d.limiter = bpsmeter.NewLimiter(0)
	d.pauseCtx, d.pauseCancel = context.WithCancel(context.Background())
	defer d.pauseCancel()

	j, m := makeJobWithArticles(t, []string{"msg1@h", "msg2@h", "msg3@h"})
	addTestJob(t, d.dispatcher, j, m)
	opts := defaultOpts(d.servers)

	calls := 0
	d.opts.HandedOff = func(x *job.Job) bool {
		calls++
		return calls > 1 // false at buildDispatchPlan's per-job gate, true afterward
	}

	plan := d.buildDispatchPlan(context.Background(), opts)
	if plan.dispatched != 3 {
		t.Fatalf("dispatched = %d, HandedOff consulted %d time(s): want 3 dispatched — "+
			"buildDispatchPlan's per-job gate runs once, before the loop, so a hand-off "+
			"landing mid-loop must not stop it from queuing the rest (this is the race "+
			"fetchArticle's own check must cover, not something buildDispatchPlan is asked to fix)",
			plan.dispatched, calls)
	}

	close(d.workCh["s1"])
	fetched := 0
	for req := range d.workCh["s1"] {
		mc := &managedConn{}
		if _, ok := d.fetchArticle(t.Context(), srv, 0, mc, req, "worker1"); ok {
			fetched++
		}
		mc.Close(d, "worker1")
	}
	if fetched != 0 {
		t.Errorf("fetchArticle fetched %d of the 3 late-queued articles, want 0 — "+
			"a handed-off job's queued articles must be dropped before fetch", fetched)
	}
	if got := ms.fetches.Load(); got != 0 {
		t.Errorf("BODY fetches = %d, want 0 — none of the late-queued articles should reach the wire", got)
	}
}
