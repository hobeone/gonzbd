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
		})
	}
}

// TestDownloader_ClearsOnlyTheInstanceAFetchWasDispatchedFor is the other
// direction: a request abandoned before a result reaches the pipeline clears
// the mark on its own instance, never on a later one whose own fetch set it.
func TestDownloader_ClearsOnlyTheInstanceAFetchWasDispatchedFor(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, d *Downloader, req *articleRequest)
	}{
		{"a dropped result", func(t *testing.T, d *Downloader, req *articleRequest) {
			d.completions <- &ArticleResult{}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			d.emitResult(ctx, req, "s1", nil, 0, 0, nil)
		}},
		{"a request drained under a global pause", func(t *testing.T, d *Downloader, req *articleRequest) {
			// A removed instance has its cancel latched, so the per-job
			// intent check drops the request before the pause check sees
			// it. An instance still at IntentRun reaches the pause check.
			stale, m := makeJobWithArticles(t, []string{"a@h"})
			if err := stale.AttachContent(m); err != nil {
				t.Fatalf("AttachContent: %v", err)
			}
			req.job = stale
			d.paused.Store(true)
			d.fetchArticle(t.Context(), d.servers[0], 0, &managedConn{}, req, "s1#0")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp, req, second := staleRequest(t)
			srv := NewServer(config.ServerConfig{Name: "s1", Enable: true, Host: "127.0.0.1", Port: 1, Connections: 1})
			d := New(disp, []*Server{srv}, nil, Options{CompletionsBuffer: 1}, slog.New(slog.DiscardHandler))
			d.pauseCtx = t.Context()
			if err := second.MarkArticleEmitted(int(req.artIdx)); err != nil {
				t.Fatalf("MarkArticleEmitted(second): %v", err)
			}

			tc.run(t, d, req)

			if !second.Progress().ArticleEmitted(int(req.artIdx)) {
				t.Errorf("the second instance's article %d lost its emitted mark to a request dispatched for the first instance", req.artIdx)
			}
		})
	}
}

// TestFetchArticle_DropsARequestForARemovedInstance: a request still queued
// for an instance that was removed is dropped before any network I/O, even
// though a later instance under the same ID is running.
func TestFetchArticle_DropsARequestForARemovedInstance(t *testing.T) {
	disp, req, second := staleRequest(t)
	var logged bytes.Buffer
	srv := NewServer(config.ServerConfig{Name: "s1", Enable: true, Host: "127.0.0.1", Port: 1, Connections: 1})
	d := New(disp, []*Server{srv}, nil, Options{},
		slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	d.pauseCtx = t.Context()
	if err := second.MarkArticleEmitted(int(req.artIdx)); err != nil {
		t.Fatalf("MarkArticleEmitted(second): %v", err)
	}

	if _, ok := d.fetchArticle(t.Context(), srv, 0, &managedConn{}, req, "s1#0"); ok {
		t.Fatal("fetchArticle fetched a body for a removed instance")
	}

	if strings.Contains(logged.String(), "msg=dialing") {
		t.Errorf("fetchArticle dialed for a request whose instance was removed; log was %q", logged.String())
	}
	if !second.Progress().ArticleEmitted(int(req.artIdx)) {
		t.Errorf("the second instance's article %d lost its emitted mark to a request dispatched for the first instance", req.artIdx)
	}
}
