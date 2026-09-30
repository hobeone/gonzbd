package downloader

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// staleRequest dispatches an article for one instance of a job, then removes
// that instance and registers a second under the same ID, the way a retry
// does once the first was finalized. It returns the request, still carrying
// the first instance, and the second instance.
func staleRequest(t *testing.T) (*dispatch.Dispatcher, *articleRequest, *job.Job) {
	t.Helper()
	disp := newTestDispatcher(t)
	first, m1 := makeJobWithArticles(t, []string{"a@h"})
	// Not ticked: a launched instance's removal waits for a worker this
	// fixture does not run.
	if err := disp.Add(context.Background(), first, dispatch.Header{Name: first.ID()}); err != nil {
		t.Fatalf("Add(first): %v", err)
	}
	if err := first.AttachContent(m1); err != nil {
		t.Fatalf("AttachContent(first): %v", err)
	}
	artIdx := artIdxFor(t, disp, first.ID(), "a@h")
	req := &articleRequest{job: first, artIdx: artIdx, messageID: "a@h", partNumber: 1}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := disp.Remove(ctx, first.ID()); err != nil {
		t.Fatalf("Remove(first): %v", err)
	}
	second, m2 := makeJobWithArticles(t, []string{"a@h"})
	addTestJob(t, disp, second, m2)
	if got, ok := disp.Job(first.ID()); !ok || got != second {
		t.Fatalf("fixture: dispatcher.Job(%s) = (%p, %v), want the second instance %p", first.ID(), got, ok, second)
	}
	return disp, req, second
}

// unreachableServer is an enabled server whose dial fails at once, so a test
// can tell from the log whether fetchArticle got as far as dialing.
func unreachableServer() *Server {
	return NewServer(config.ServerConfig{Name: "s1", Enable: true, Host: "127.0.0.1", Port: 1, Connections: 1})
}

// markEmittedOn sets j's Emitted bit for artIdx, failing the test if it cannot.
func markEmittedOn(t *testing.T, j *job.Job, artIdx int32) {
	t.Helper()
	if err := j.MarkArticleEmitted(int(artIdx)); err != nil {
		t.Fatalf("MarkArticleEmitted(%p): %v", j, err)
	}
}

// TestDownloader_MarksOnlyTheInstanceAFetchWasDispatchedFor: every Emitted
// mark the downloader makes for a request lands on the instance the request
// was dispatched for. A mark on a later instance under the same ID claims a
// result is on its way to it, which the pipeline then drops as stale, so
// nothing ever clears the mark and the article is never dispatched again.
func TestDownloader_MarksOnlyTheInstanceAFetchWasDispatchedFor(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, d *Downloader, req *articleRequest)
	}{
		{"a successful fetch", func(t *testing.T, d *Downloader, req *articleRequest) {
			d.processFetchedArticle(t.Context(), NewServer(config.ServerConfig{Name: "s1"}), req,
				yencBody("test.bin", []byte("payload")))
		}},
		{"a terminal decode error", func(t *testing.T, d *Downloader, req *articleRequest) {
			d.processFetchedArticle(t.Context(), NewServer(config.ServerConfig{Name: "s1"}), req,
				[]byte("neither yEnc nor UU\r\n"))
		}},
		{"an exhausted try-list", func(t *testing.T, d *Downloader, req *articleRequest) {
			d.applyDispatchPlan(t.Context(), dispatchPlan{exhausted: []*articleRequest{req}}, dispatchOpts{})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp, req, second := staleRequest(t)
			d := New(disp, nil, nil, Options{}, slog.New(slog.DiscardHandler))

			tc.run(t, d, req)

			if second.Progress().ArticleEmitted(int(req.artIdx)) {
				t.Errorf("the second instance's article %d is marked emitted by a fetch dispatched for the first instance", req.artIdx)
			}
			if !req.job.Progress().ArticleEmitted(int(req.artIdx)) {
				t.Errorf("the request's own instance's article %d is not marked emitted after a result was sent for it", req.artIdx)
			}
		})
	}
}

// TestEmitResult_ClearsOnlyTheInstanceADroppedResultWasFor is the other
// direction: a result dropped before it reaches the pipeline clears the mark
// on its own instance, never on a later one whose own fetch set it.
func TestEmitResult_ClearsOnlyTheInstanceADroppedResultWasFor(t *testing.T) {
	disp, req, second := staleRequest(t)
	d := New(disp, nil, nil, Options{CompletionsBuffer: 1}, slog.New(slog.DiscardHandler))
	markEmittedOn(t, req.job, req.artIdx)
	markEmittedOn(t, second, req.artIdx)

	d.completions <- &ArticleResult{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d.emitResult(ctx, req, "s1", nil, 0, 0, nil)

	if !second.Progress().ArticleEmitted(int(req.artIdx)) {
		t.Errorf("the second instance's article %d lost its emitted mark to a request dispatched for the first instance", req.artIdx)
	}
	if req.job.Progress().ArticleEmitted(int(req.artIdx)) {
		t.Errorf("the request's own instance's article %d is still marked emitted after its result was dropped", req.artIdx)
	}
}

// TestFetchArticle_DropsARequestForAnInstanceNoLongerRegistered: a request
// still queued for an instance that is no longer the one registered under its
// ID is dropped before any network I/O, even though a later instance under
// the same ID is running, and the drop clears only its own instance's mark.
//
// Both ways an instance leaves the registry are covered: removal, which
// latches its cancel, and an instance still at IntentRun, which only the
// identity comparison catches.
func TestFetchArticle_DropsARequestForAnInstanceNoLongerRegistered(t *testing.T) {
	cases := []struct {
		name  string
		stale func(t *testing.T, req *articleRequest)
	}{
		{"a removed instance", func(*testing.T, *articleRequest) {}},
		{"a superseded instance still at IntentRun", func(t *testing.T, req *articleRequest) {
			stale, m := makeJobWithArticles(t, []string{"a@h"})
			if err := stale.AttachContent(m); err != nil {
				t.Fatalf("AttachContent: %v", err)
			}
			req.job = stale
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp, req, second := staleRequest(t)
			tc.stale(t, req)
			var logged bytes.Buffer
			srv := unreachableServer()
			d := New(disp, []*Server{srv}, nil, Options{},
				slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
			d.pauseCtx = t.Context()
			markEmittedOn(t, req.job, req.artIdx)
			markEmittedOn(t, second, req.artIdx)

			if _, ok := d.fetchArticle(t.Context(), srv, 0, &managedConn{}, req, "s1#0"); ok {
				t.Fatal("fetchArticle fetched a body for an instance that is no longer registered")
			}

			if strings.Contains(logged.String(), "msg=dialing") {
				t.Errorf("fetchArticle dialed for a request whose instance is no longer registered; log was %q", logged.String())
			}
			if !second.Progress().ArticleEmitted(int(req.artIdx)) {
				t.Errorf("the second instance's article %d lost its emitted mark to a request dispatched for another instance", req.artIdx)
			}
			if req.job.Progress().ArticleEmitted(int(req.artIdx)) {
				t.Errorf("the request's own instance's article %d is still marked emitted after the request was dropped", req.artIdx)
			}
		})
	}
}

// TestFetchArticle_GlobalPauseClearsTheRequestsOwnMark: a request drained
// under a global pause is returned to the dispatch pool on its own instance.
// It reaches the pause check only when its instance is the registered one;
// the check above it drops every other.
func TestFetchArticle_GlobalPauseClearsTheRequestsOwnMark(t *testing.T) {
	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, []string{"a@h"})
	addTestJob(t, disp, j, m)
	artIdx := artIdxFor(t, disp, j.ID(), "a@h")
	req := &articleRequest{job: j, artIdx: artIdx, messageID: "a@h", partNumber: 1}
	srv := unreachableServer()
	d := New(disp, []*Server{srv}, nil, Options{}, slog.New(slog.DiscardHandler))
	d.pauseCtx = t.Context()
	d.paused.Store(true)
	markEmittedOn(t, j, artIdx)

	if _, ok := d.fetchArticle(t.Context(), srv, 0, &managedConn{}, req, "s1#0"); ok {
		t.Fatal("fetchArticle fetched a body under a global pause")
	}

	if j.Progress().ArticleEmitted(int(artIdx)) {
		t.Errorf("article %d is still marked emitted after a global pause drained its request, so it is never re-dispatched", artIdx)
	}
}

// TestArticleRequestAndResult_DeriveTheirIDFromTheInstance: neither the
// request nor the result stores a job ID of its own, so the ID a consumer
// reads is always the ID of the instance it carries.
func TestArticleRequestAndResult_DeriveTheirIDFromTheInstance(t *testing.T) {
	j := bareJob("j-derived")
	req := &articleRequest{job: j, artIdx: 3, messageID: "a@h"}
	if got := req.jobID(); got != "j-derived" {
		t.Errorf("req.jobID() = %q, want the instance's ID %q", got, "j-derived")
	}

	d := New(newTestDispatcher(t), nil, nil, Options{CompletionsBuffer: 1}, slog.New(slog.DiscardHandler))
	d.emitResult(t.Context(), req, "s1", nil, 0, 0, nil)
	res := <-d.Completions()
	if res.Job != j {
		t.Errorf("result carries instance %p, want the request's %p", res.Job, j)
	}
	if got := res.JobID(); got != "j-derived" {
		t.Errorf("res.JobID() = %q, want the instance's ID %q", got, "j-derived")
	}
}

// TestMarkEmitted_ReportsAMarkThatFailed: a mark that cannot be recorded is
// logged rather than dropped silently, since the article would otherwise be
// re-dispatched while its result is still on its way.
func TestMarkEmitted_ReportsAMarkThatFailed(t *testing.T) {
	var logged bytes.Buffer
	d := New(newTestDispatcher(t), nil, nil, Options{},
		slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))

	// A job with no content attached cannot record the mark.
	d.markEmitted(&articleRequest{job: bareJob("j-bare"), artIdx: 0, messageID: "a@h"})

	if !strings.Contains(logged.String(), "mark article emitted failed") {
		t.Errorf("a failed mark was not reported; log was %q", logged.String())
	}
}
