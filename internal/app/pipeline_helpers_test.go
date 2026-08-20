package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestRefFor_CarriesEveryIdentityField pins that refFor copies all four
// identity fields off the result.
//
// The failure it guards is a field being added to assembler.ArticleRef and
// silently left unset here — the two write paths would both submit articles
// with a zero in it, agreeing with each other and so breaking no other test.
// Asserting on the whole struct rather than field by field is what makes a
// new field fail this: a field-by-field check would keep passing.
func TestRefFor_CarriesEveryIdentityField(t *testing.T) {
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

// helperJob builds a queue with one job of nFiles files and nArticles
// articles each, and returns both.
func helperJob(t *testing.T, name string, nFiles, nArticles int) (*queue.Queue, *queue.Job) {
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
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: name + ".nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := queue.New()
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return q, job
}

func helperPipeline(t *testing.T, q *queue.Queue) *pipeline {
	t.Helper()
	return &pipeline{
		log:         slog.Default(),
		queue:       q,
		downloadDir: t.TempDir(),
		fileInfo:    make(map[fileKey]assembler.FileInfo),
	}
}

func TestResolveFileInfo(t *testing.T) {
	q, job := helperJob(t, "resolve", 1, 2)
	p := helperPipeline(t, q)

	t.Run("errors for an unregistered file", func(t *testing.T) {
		// The assembler discards the article on error, so this must not
		// succeed with a zero FileInfo — an empty Path would write to the
		// process's working directory.
		if _, err := p.resolveFileInfo(job.ID, 0); err == nil {
			t.Error("resolveFileInfo succeeded before the file was registered")
		}
	})

	t.Run("returns the registered entry", func(t *testing.T) {
		if err := p.registerFile(job.ID, 0); err != nil {
			t.Fatalf("registerFile: %v", err)
		}
		info, err := p.resolveFileInfo(job.ID, 0)
		if err != nil {
			t.Fatalf("resolveFileInfo: %v", err)
		}
		if info.Path == "" {
			t.Error("resolved FileInfo has an empty Path")
		}
	})

	t.Run("does not confuse two file indices", func(t *testing.T) {
		if _, err := p.resolveFileInfo(job.ID, 1); err == nil {
			t.Error("file 1 resolved from file 0's entry")
		}
	})
}

func TestForgetJob(t *testing.T) {
	// The cache would otherwise grow for the lifetime of the process, one
	// entry per file of every job ever downloaded.
	qA, jobA := helperJob(t, "forgetA", 2, 1)
	p := helperPipeline(t, qA)
	for fi := range 2 {
		if err := p.registerFile(jobA.ID, fi); err != nil {
			t.Fatalf("registerFile: %v", err)
		}
	}
	// A second job registered through the same pipeline, to pin that
	// forgetJob removes one job's entries and not the whole map.
	_, jobB := helperJob(t, "forgetB", 1, 1)
	p.fileInfo[fileKey{jobID: jobB.ID, fileIdx: 0}] = assembler.FileInfo{Path: "/tmp/b"}

	if len(p.fileInfo) != 3 {
		t.Fatalf("fileInfo has %d entries, want 3", len(p.fileInfo))
	}

	p.forgetJob(jobA.ID)

	if _, ok := p.fileInfo[fileKey{jobID: jobA.ID, fileIdx: 0}]; ok {
		t.Error("jobA file 0 survived forgetJob")
	}
	if _, ok := p.fileInfo[fileKey{jobID: jobA.ID, fileIdx: 1}]; ok {
		t.Error("jobA file 1 survived forgetJob")
	}
	if _, ok := p.fileInfo[fileKey{jobID: jobB.ID, fileIdx: 0}]; !ok {
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

// TestHandleResult_RoutesOnError pins handleResult's only decision: an
// ArticleResult carrying an error goes to handleFailureResult and one without
// goes to handleSuccessResult.
//
// The discriminator is the job's download-started timestamp. handleSuccessResult
// sets it through Queue.MarkJobStarted before doing anything else, and nothing
// on the failure path touches it — so it is set if and only if the success
// branch ran. An earlier version of this test asserted only that registerFile
// had run and the heartbeat had fired, which both branches do; it passed
// against a handleResult whose two arms both called handleSuccessResult, i.e.
// against precisely the defect its own comment named.
//
// Each subtest builds its own job. MarkJobStarted is first-start-wins and
// never resets, so a shared fixture would make the assertions order-dependent.
func TestHandleResult_RoutesOnError(t *testing.T) {
	newCase := func(t *testing.T, name string) (*pipeline, *queue.Job) {
		t.Helper()
		q, job := helperJob(t, name, 1, 2)
		p := helperPipeline(t, q)
		p.assembler = assembler.New(assembler.Options{FileInfo: p.resolveFileInfo}, nil)
		return p, job
	}
	startedAt := func(t *testing.T, p *pipeline, jobID string) time.Time {
		t.Helper()
		snap := p.queue.SnapshotJob(jobID)
		if snap == nil {
			t.Fatalf("job %s vanished", jobID)
		}
		return snap.Progress().DownloadStarted()
	}

	t.Run("an error takes the failure path", func(t *testing.T) {
		p, job := newCase(t, "route-fail")
		p.handleResult(t.Context(), &downloader.ArticleResult{
			JobID:     job.ID,
			FileIdx:   0,
			MessageID: "route-fail-f0-a0@x",
			Err:       downloader.ErrNoServersLeft,
			Subject:   "file0.rar",
		})
		if got := startedAt(t, p, job.ID); !got.IsZero() {
			t.Errorf("download-started = %v after a failed article, want zero — the failure "+
				"was routed to handleSuccessResult, which records it as downloaded data", got)
		}
		// The failure path is terminal here (ErrNoServersLeft), so it does
		// reach registerFile. Asserting this too keeps the test honest about
		// which path ran rather than only which one did not.
		if _, err := p.resolveFileInfo(job.ID, 0); err != nil {
			t.Errorf("the terminal failure path did not register the file: %v", err)
		}
	})

	t.Run("no error takes the success path", func(t *testing.T) {
		p, job := newCase(t, "route-ok")
		p.handleResult(t.Context(), &downloader.ArticleResult{
			JobID:     job.ID,
			FileIdx:   0,
			MessageID: "route-ok-f0-a0@x",
			Subject:   "file0.rar",
			Data:      []byte("payload"),
		})
		if startedAt(t, p, job.ID).IsZero() {
			t.Error("download-started is zero after a successful article — the success " +
				"was routed to handleFailureResult")
		}
	})

	// The heartbeat is the watchdog's liveness signal and sits above the
	// branch, so it must fire either way: a job failing every article is
	// still making progress and must not be killed as stalled.
	t.Run("fires the heartbeat for a failure", func(t *testing.T) {
		p, job := newCase(t, "beat-fail")
		var beats int
		p.onHeartbeat = func() { beats++ }
		p.handleResult(t.Context(), &downloader.ArticleResult{
			JobID: job.ID, FileIdx: 0, MessageID: "beat-fail-f0-a0@x",
			Err: downloader.ErrNoServersLeft, Subject: "file0.rar",
		})
		if beats != 1 {
			t.Errorf("onHeartbeat fired %d times for a failure, want 1", beats)
		}
	})

	t.Run("fires the heartbeat for a success", func(t *testing.T) {
		p, job := newCase(t, "beat-ok")
		var beats int
		p.onHeartbeat = func() { beats++ }
		p.handleResult(t.Context(), &downloader.ArticleResult{
			JobID: job.ID, FileIdx: 0, MessageID: "beat-ok-f0-a0@x",
			Subject: "file0.rar", Data: []byte("payload"),
		})
		if beats != 1 {
			t.Errorf("onHeartbeat fired %d times for a success, want 1", beats)
		}
	})
}

// TestAwaitInFlight covers both of awaitInFlight's exits directly. The swap
// test above drives it through run(), which exercises only the success arm —
// the cancellation arm is what stops a wedged write from blocking a config
// reload forever, so it needs its own case rather than inheriting coverage.
func TestAwaitInFlight(t *testing.T) {
	t.Run("returns true once the outstanding work is done", func(t *testing.T) {
		p := &pipeline{log: slog.New(slog.DiscardHandler)}
		// Go adds before it returns, so the counter is already 1 when the
		// wait below starts — the Add/Done pair this replaces had to be
		// written out for the same guarantee.
		p.inFlight.Go(func() { time.Sleep(10 * time.Millisecond) })
		if !p.awaitInFlight(t.Context()) {
			t.Error("awaitInFlight reported cancellation for work that completed")
		}
	})

	t.Run("returns false when the context ends first", func(t *testing.T) {
		p := &pipeline{log: slog.New(slog.DiscardHandler)}
		p.inFlight.Add(1)
		// Released at the end so the Wait goroutine this spawns can exit;
		// leaving it blocked would leak it for the rest of the test binary.
		defer p.inFlight.Done()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if p.awaitInFlight(ctx) {
			t.Error("awaitInFlight reported completion while an item was still in flight")
		}
	})
}

// TestSetCompletions_WaitsForQueuedWritesNotJustTheChannel pins the half of
// the swap contract that used to be a comment: setCompletions(nil) must not
// return while results it drained are still queued for a worker.
//
// This runs the REAL run() loop rather than a stand-in, because the defect
// lived in run(): the drain loop moves results from the completions channel
// onto a buffered work channel and the pwrite happens later, so "the channel
// is empty" and "the work is done" are different facts. ReloadDownloader
// reads setCompletions(nil) as a quiescence point and checkpoints against it,
// which is why the difference is #390 and not a nicety.
//
// The failure results take the retryable path, which needs no assembler — the
// point under test is the run loop's accounting, not what a worker does.
func TestSetCompletions_WaitsForQueuedWritesNotJustTheChannel(t *testing.T) {
	const results = 16
	const perResult = 5 * time.Millisecond

	var handled atomic.Int64
	p := &pipeline{
		log:        slog.New(slog.DiscardHandler),
		queue:      queue.New(),
		fileInfo:   make(map[fileKey]assembler.FileInfo),
		updateCh:   make(chan completionSwap, 1),
		numWorkers: 1,
		// Deliberately slow, so the workers cannot plausibly finish inside
		// the window where an unfixed setCompletions returns. One worker and
		// 16 results is ~80ms of work against a swap that would otherwise
		// complete in microseconds.
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
		t.Errorf("setCompletions returned with %d of %d results still unprocessed — "+
			"it drained the channel but not the work, so a caller treating the swap "+
			"as a quiescence point (ReloadDownloader, before its checkpoint) acks "+
			"less than it believes and #390 survives the fix", results-got, results)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not exit after its context was cancelled")
	}
}

func TestSetCompletions(t *testing.T) {
	t.Run("swaps the channel and blocks until run acknowledges", func(t *testing.T) {
		p := &pipeline{
			log:      slog.Default(),
			updateCh: make(chan completionSwap),
			fileInfo: make(map[fileKey]assembler.FileInfo),
		}
		p.ctx = t.Context()

		// Stand in for run(): accept the swap and close its done channel.
		// setCompletions must not return before that happens, or the caller
		// can start feeding a channel the reader has not adopted yet.
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
		// Nothing is reading updateCh here, which is the state after run()
		// has exited. Without the ctx arm this would deadlock the caller.
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
		// The swap is accepted but run() dies before closing done. The second
		// select's ctx arm is the only thing that releases the caller.
		ctx, cancel := context.WithCancel(context.Background())
		p := &pipeline{
			log:      slog.Default(),
			ctx:      ctx,
			updateCh: make(chan completionSwap),
			fileInfo: make(map[fileKey]assembler.FileInfo),
		}
		go func() {
			<-p.updateCh // accept, then never ack
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
