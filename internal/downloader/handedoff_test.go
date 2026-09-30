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
//
// The same gate (dispatch.go:646-658) also unmarks the server the dropped
// request was tried on. That is not specific to a hand-off: the gate is
// shared with the pause/cancel/superseded-instance checks above it, and it
// exists so that a job paused mid-fetch and later resumed does not find the
// server its drained request never reached permanently marked tried
// (TestDownloaderPerJobPauseResume exercises that resume path directly; this
// test pins the clear itself by reading the tracker). Pre-marking servers 0
// and 1 tried and asserting that only server 0 — the one the dropped request
// was on — clears, while server 1 stays marked, is what a single marked
// server cannot do: with one bit set, unmarkTried (clear that one server)
// and clearTried (clear the whole entry) leave the same empty result, so a
// mutation from one to the other would pass unnoticed.
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

	artIdx := artIdxFor(t, d.dispatcher, j.ID(), "a@h")
	req := &articleRequest{job: j, artIdx: artIdx, messageID: "a@h"}
	// Pre-set the emitted bit so the assertion below actually exercises
	// ClearArticleEmitted, the way TestFetchArticle_DropsARequestForAn
	// InstanceNoLongerRegistered (instance_test.go) does: a fresh job's
	// article starts unemitted, so checking ArticleEmitted without this
	// would pass whether or not the drop path clears anything.
	markEmittedOn(t, j, artIdx)

	// Pre-mark the article as tried on servers 0 and 1, the way a real
	// dispatch pass would before offering it to fetchArticle.
	key := testArticleKey(j, artIdx)
	var mask serverMask
	mask.set(0)
	mask.set(1)
	d.tracker.Lock()
	d.tracker.SetTriedLocked(key, mask)
	d.tracker.Unlock()

	mc := &managedConn{}
	defer mc.Close(d, "worker1")
	body, ok := d.fetchArticle(t.Context(), srv, 0, mc, req, "worker1")
	if ok || body != nil {
		t.Errorf("fetchArticle = (%v, %v), want (nil, false) for a handed-off job", body, ok)
	}
	if got := ms.fetches.Load(); got != 0 {
		t.Errorf("fetchArticle for a handed-off job: ok=%v, BODY fetches=%d, want no fetch", ok, got)
	}
	if j.Progress().ArticleEmitted(int(artIdx)) {
		t.Errorf("article %d is still marked emitted after a handed-off job's request was dropped", artIdx)
	}

	d.tracker.Lock()
	gotMask, stillTried := d.tracker.TryListLocked(key)
	d.tracker.Unlock()
	if !stillTried {
		t.Fatalf("try-list entry for article %d is gone after the drop, want server 1 still marked tried", artIdx)
	}
	if gotMask.has(0) {
		t.Errorf("server 0 is still marked tried after fetchArticle dropped the request that was dispatched to it")
	}
	if !gotMask.has(1) {
		t.Errorf("server 1's tried mark was cleared by a drop dispatched to server 0, want only server 0's mark cleared")
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
	t.Parallel()

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
