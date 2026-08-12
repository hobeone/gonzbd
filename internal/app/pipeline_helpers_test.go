package app

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

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
