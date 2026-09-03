package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestRefFor_CarriesEveryIdentityField pins that refFor copies all four
// identity fields off the result.
func TestRefFor_CarriesEveryIdentityField(t *testing.T) {
	t.Parallel()
	res := &downloader.ArticleResult{
		JobID:     "job-7",
		FileIdx:   4,
		ArtIdx:    11,
		MessageID: "a@b",
	}

	want := assembler.ArticleRef{
		JobID:     "job-7",
		FileIdx:   4,
		ArtIdx:    11,
		MessageID: "a@b",
	}
	if got := refFor(res); got != want {
		t.Errorf("refFor(%+v) = %+v, want %+v", res, got, want)
	}
}

// helperJob builds a dispatcher with one job of nFiles files and nArticles
// articles each, and returns both.
func helperJob(t *testing.T, app *Application, name string, nFiles, nArticles int) (*dispatch.Dispatcher, *job.Job) {
	t.Helper()
	parsed := &nzb.NZB{Meta: map[string][]string{"title": {name}}}
	for fi := range nFiles {
		f := nzb.File{Subject: fmt.Sprintf("file%d.rar", fi), Bytes: int64(nArticles) * 1000}
		for ai := range nArticles {
			f.Articles = append(f.Articles, nzb.Article{
				ID:     fmt.Sprintf("%s-f%d-a%d@x", name, fi, ai),
				Number: ai + 1,
				Bytes:  1000,
			})
		}
		parsed.Files = append(parsed.Files, f)
	}
	j, hdr, err := BuildIngestJob(app.config, parsed, name+".nzb", types.FetchOptions{NzbName: name + ".nzb"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := app.Dispatcher().Add(j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return app.Dispatcher(), j
}

func helperPipeline(t *testing.T, d *dispatch.Dispatcher) *pipeline {
	t.Helper()
	return &pipeline{
		log:         slog.Default(),
		dispatcher:  d,
		downloadDir: t.TempDir(),
		fileInfo:    make(map[fileKey]assembler.FileInfo),
	}
}

func TestResolveFileInfo(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)
	disp, j := helperJob(t, app, "resolve", 1, 2)
	p := helperPipeline(t, disp)

	t.Run("errors for an unregistered file", func(t *testing.T) {
		if _, err := p.resolveFileInfo(j.ID(), 0); err == nil {
			t.Error("resolveFileInfo succeeded before the file was registered")
		}
	})

	t.Run("returns the registered entry", func(t *testing.T) {
		if err := p.registerFile(j.ID(), 0); err != nil {
			t.Fatalf("registerFile: %v", err)
		}
		info, err := p.resolveFileInfo(j.ID(), 0)
		if err != nil {
			t.Fatalf("resolveFileInfo: %v", err)
		}
		if info.Path == "" {
			t.Error("resolved FileInfo has an empty Path")
		}
	})

	t.Run("does not confuse two file indices", func(t *testing.T) {
		if _, err := p.resolveFileInfo(j.ID(), 1); err == nil {
			t.Error("file 1 resolved from file 0's entry")
		}
	})
}

func TestForgetJob(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)
	dispA, jobA := helperJob(t, app, "forgetA", 2, 1)
	p := helperPipeline(t, dispA)
	for fi := range 2 {
		if err := p.registerFile(jobA.ID(), fi); err != nil {
			t.Fatalf("registerFile: %v", err)
		}
	}
	_, jobB := helperJob(t, app, "forgetB", 1, 1)
	p.fileInfo[fileKey{jobID: jobB.ID(), fileIdx: 0}] = assembler.FileInfo{Path: "/tmp/b"}

	if len(p.fileInfo) != 3 {
		t.Fatalf("fileInfo has %d entries, want 3", len(p.fileInfo))
	}

	p.forgetJob(jobA.ID())

	if _, ok := p.fileInfo[fileKey{jobID: jobA.ID(), fileIdx: 0}]; ok {
		t.Error("jobA file 0 survived forgetJob")
	}
	if _, ok := p.fileInfo[fileKey{jobID: jobA.ID(), fileIdx: 1}]; ok {
		t.Error("jobA file 1 survived forgetJob")
	}
	if _, ok := p.fileInfo[fileKey{jobID: jobB.ID(), fileIdx: 0}]; !ok {
		t.Error("forgetJob removed another job's entry")
	}

	t.Run("unknown job is a no-op", func(t *testing.T) {
		before := len(p.fileInfo)
		p.forgetJob("no-such-job")
		if len(p.fileInfo) != before {
			t.Errorf("fileInfo went from %d to %d entries for an unknown job", before, len(p.fileInfo))
		}
	})
}

func TestHandleResult_RoutesOnError(t *testing.T) {
	t.Parallel()
	newCase := func(t *testing.T, name string) (*pipeline, *job.Job) {
		t.Helper()
		app := newTestApplication(t)
		disp, j := helperJob(t, app, name, 1, 2)
		p := helperPipeline(t, disp)
		p.assembler = assembler.New(assembler.Options{FileInfo: p.resolveFileInfo}, nil)
		return p, j
	}
	startedAt := func(t *testing.T, p *pipeline, jobID string) time.Time {
		t.Helper()
		j, ok := p.dispatcher.Job(jobID)
		if !ok {
			t.Fatalf("job %s vanished", jobID)
		}
		return j.Progress().DownloadStarted()
	}

	t.Run("an error takes the failure path", func(t *testing.T) {
		p, j := newCase(t, "route-fail")
		p.handleResult(t.Context(), &downloader.ArticleResult{
			JobID:     j.ID(),
			FileIdx:   0,
			MessageID: "route-fail-f0-a0@x",
			Err:       downloader.ErrNoServersLeft,
			Subject:   "file0.rar",
		})
		if got := startedAt(t, p, j.ID()); !got.IsZero() {
			t.Errorf("download-started = %v after a failed article, want zero", got)
		}
		if _, err := p.resolveFileInfo(j.ID(), 0); err != nil {
			t.Errorf("the terminal failure path did not register the file: %v", err)
		}
	})

	t.Run("no error takes the success path", func(t *testing.T) {
		p, j := newCase(t, "route-ok")
		p.handleResult(t.Context(), &downloader.ArticleResult{
			JobID:     j.ID(),
			FileIdx:   0,
			MessageID: "route-ok-f0-a0@x",
			Subject:   "file0.rar",
			Data:      []byte("payload"),
		})
		if startedAt(t, p, j.ID()).IsZero() {
			t.Error("download-started is zero after a successful article")
		}
	})

	t.Run("fires the heartbeat for a failure", func(t *testing.T) {
		p, j := newCase(t, "beat-fail")
		var beats int
		p.onHeartbeat = func() { beats++ }
		p.handleResult(t.Context(), &downloader.ArticleResult{
			JobID: j.ID(), FileIdx: 0, MessageID: "beat-fail-f0-a0@x",
			Err: downloader.ErrNoServersLeft, Subject: "file0.rar",
		})
		if beats != 1 {
			t.Errorf("onHeartbeat fired %d times for a failure, want 1", beats)
		}
	})

	t.Run("fires the heartbeat for a success", func(t *testing.T) {
		p, j := newCase(t, "beat-ok")
		var beats int
		p.onHeartbeat = func() { beats++ }
		p.handleResult(t.Context(), &downloader.ArticleResult{
			JobID: j.ID(), FileIdx: 0, MessageID: "beat-ok-f0-a0@x",
			Subject: "file0.rar", Data: []byte("payload"),
		})
		if beats != 1 {
			t.Errorf("onHeartbeat fired %d times for a success, want 1", beats)
		}
	})
}

func TestAwaitInFlight(t *testing.T) {
	t.Parallel()
	t.Run("returns true once the outstanding work is done", func(t *testing.T) {
		p := &pipeline{log: slog.New(slog.DiscardHandler)}
		p.inFlight.Go(func() { time.Sleep(10 * time.Millisecond) })
		if !p.awaitInFlight(t.Context()) {
			t.Error("awaitInFlight reported cancellation for work that completed")
		}
	})

	t.Run("returns false when the context ends first", func(t *testing.T) {
		p := &pipeline{log: slog.New(slog.DiscardHandler)}
		p.inFlight.Add(1)
		defer p.inFlight.Done()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if p.awaitInFlight(ctx) {
			t.Error("awaitInFlight reported completion while an item was still in flight")
		}
	})
}

func TestSetCompletions_WaitsForQueuedWritesNotJustTheChannel(t *testing.T) {
	t.Parallel()
	const results = 16
	const perResult = 5 * time.Millisecond

	var handled atomic.Int64
	p := &pipeline{
		log:        slog.New(slog.DiscardHandler),
		fileInfo:   make(map[fileKey]assembler.FileInfo),
		updateCh:   make(chan completionSwap, 1),
		numWorkers: 1,
		onHeartbeat: func() {
			time.Sleep(perResult)
			handled.Add(1)
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.ctx = ctx

	comp := make(chan *downloader.ArticleResult, results)
	for i := range results {
		comp <- &downloader.ArticleResult{
			JobID:  "no-such-job",
			ArtIdx: int32(i), //nolint:gosec // loop bound is 16
			Err:    nntp.ErrNoArticle,
		}
	}
	p.completions = comp

	runDone := make(chan struct{})
	go func() { p.run(ctx); close(runDone) }()

	p.setCompletions(nil)

	if got := handled.Load(); got != results {
		t.Errorf("setCompletions returned with %d of %d results still unprocessed", results-got, results)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not exit after its context was cancelled")
	}
}

func TestSetCompletions(t *testing.T) {
	t.Parallel()
	t.Run("swaps the channel and blocks until run acknowledges", func(t *testing.T) {
		p := &pipeline{
			log:      slog.Default(),
			updateCh: make(chan completionSwap),
			fileInfo: make(map[fileKey]assembler.FileInfo),
		}
		p.ctx = t.Context()

		got := make(chan *downloader.ArticleResult, 1)
		accepted := make(chan completionSwap, 1)
		go func() {
			sw := <-p.updateCh
			accepted <- sw
			close(sw.done)
		}()

		p.setCompletions(got)

		select {
		case sw := <-accepted:
			if sw.ch == nil {
				t.Error("swap carried a nil channel")
			}
		default:
			t.Error("setCompletions returned without the swap being accepted")
		}
	})

	t.Run("returns rather than blocking when the context is done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := &pipeline{
			log:      slog.Default(),
			ctx:      ctx,
			updateCh: make(chan completionSwap),
			fileInfo: make(map[fileKey]assembler.FileInfo),
		}
		done := make(chan struct{})
		go func() { p.setCompletions(nil); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("setCompletions blocked forever after the context was cancelled")
		}
	})

	t.Run("returns when the context is cancelled while awaiting the ack", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		p := &pipeline{
			log:      slog.Default(),
			ctx:      ctx,
			updateCh: make(chan completionSwap),
			fileInfo: make(map[fileKey]assembler.FileInfo),
		}
		go func() {
			<-p.updateCh
			cancel()
		}()
		done := make(chan struct{})
		go func() { p.setCompletions(nil); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("setCompletions blocked forever waiting for an ack that never came")
		}
	})
}
