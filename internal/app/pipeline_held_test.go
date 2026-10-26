package app

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/heldbytes"
	"github.com/hobeone/gonzbd/internal/job"
)

// heldResult is a successful result for article 0 of file fileIdx of j whose
// data is charged against budget, as the downloader charges every decode.
func heldResult(t *testing.T, budget *heldbytes.Budget, j *job.Job, fileIdx int) *downloader.ArticleResult {
	t.Helper()
	held, err := budget.Acquire(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	data := append(decoder.GetBuffer(100), "payload"...)
	return &downloader.ArticleResult{
		Job: j, FileIdx: fileIdx, ArtIdx: 0, MessageID: "a@x",
		Data: data, ServerName: "s1", Held: held,
	}
}

func requireNothingHeld(t *testing.T, budget *heldbytes.Budget, path string) {
	t.Helper()
	if got := budget.Held(); got != 0 {
		t.Fatalf("%s: Held() = %d, want 0: the budget stays charged for an article nothing holds", path, got)
	}
}

// TestPipeline_GivesBackTheBudgetOnEveryPath: each way a result leaves the
// pipeline — dropped, failed, refused by registration or by the assembler, or
// written — ends with its bytes back in the downloader's budget.
func TestPipeline_GivesBackTheBudgetOnEveryPath(t *testing.T) {
	t.Parallel()

	newPipeline := func(t *testing.T, app *Application, a *assembler.Assembler) *pipeline {
		return &pipeline{
			log:         slog.New(slog.DiscardHandler),
			dispatcher:  app.Dispatcher(),
			downloadDir: t.TempDir(),
			assembler:   a,
			fileInfo:    make(map[fileKey]fileEntry),
		}
	}
	unstarted := func() *assembler.Assembler {
		return assembler.New(assembler.Options{
			FileInfo: func(assembler.Owner, int) (assembler.FileInfo, error) { return assembler.FileInfo{}, nil },
		}, slog.New(slog.DiscardHandler))
	}

	t.Run("a result for an instance no longer registered", func(t *testing.T) {
		t.Parallel()
		app := newTestApplication(t)
		budget := heldbytes.New(1 << 20)
		p := newPipeline(t, app, unstarted())
		p.handleResult(t.Context(), heldResult(t, budget, job.New("gone", "gone.nzb", job.Policy{}), 0))
		requireNothingHeld(t, budget, "dropped as not current")
	})

	t.Run("a failed result", func(t *testing.T) {
		t.Parallel()
		app := newTestApplication(t)
		_, j := helperJob(t, app, "heldfail", 1, 1)
		budget := heldbytes.New(1 << 20)
		p := newPipeline(t, app, unstarted())
		res := heldResult(t, budget, j, 0)
		res.Err = errors.New("retry me")
		p.handleResult(t.Context(), res)
		requireNothingHeld(t, budget, "failure")
	})

	t.Run("a file that cannot be registered", func(t *testing.T) {
		t.Parallel()
		app := newTestApplication(t)
		_, j := helperJob(t, app, "heldreg", 1, 1)
		budget := heldbytes.New(1 << 20)
		p := newPipeline(t, app, unstarted())
		p.handleResult(t.Context(), heldResult(t, budget, j, 9)) // no file 9
		requireNothingHeld(t, budget, "registration failure")
	})

	t.Run("an assembler that refuses the write", func(t *testing.T) {
		t.Parallel()
		app := newTestApplication(t)
		_, j := helperJob(t, app, "heldrefused", 1, 1)
		budget := heldbytes.New(1 << 20)
		p := newPipeline(t, app, unstarted()) // never started: WriteArticle fails
		p.handleResult(t.Context(), heldResult(t, budget, j, 0))
		requireNothingHeld(t, budget, "WriteArticle error")
	})

	t.Run("a written article", func(t *testing.T) {
		t.Parallel()
		app := newTestApplication(t)
		_, j := helperJob(t, app, "heldwritten", 1, 2)
		budget := heldbytes.New(1 << 20)
		dir := t.TempDir()
		a := assembler.New(assembler.Options{
			FileInfo: func(assembler.Owner, int) (assembler.FileInfo, error) {
				return assembler.FileInfo{Dir: dir, Name: "f.bin", TotalParts: 2}, nil
			},
		}, slog.New(slog.DiscardHandler))
		if err := a.Start(t.Context()); err != nil {
			t.Fatalf("assembler.Start: %v", err)
		}
		t.Cleanup(func() { _ = a.Stop() })
		p := newPipeline(t, app, a)

		p.handleResult(t.Context(), heldResult(t, budget, j, 0))
		if err := a.Quiesce(t.Context()); err != nil {
			t.Fatalf("Quiesce: %v", err)
		}
		requireNothingHeld(t, budget, "written by the assembler")
	})
}

// TestPipeline_GivesBackTheBudgetOfAResultDroppedAtShutdown: a result run()
// has taken from completions but cannot hand to a busy worker before its
// context ends is released by run() itself, at once, not left charged.
func TestPipeline_GivesBackTheBudgetOfAResultDroppedAtShutdown(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)
	_, j := helperJob(t, app, "helddrop", 1, 1)
	budget := heldbytes.New(1 << 20)
	completions := make(chan *downloader.ArticleResult)
	p := &pipeline{
		log:         slog.New(slog.DiscardHandler),
		dispatcher:  app.Dispatcher(),
		downloadDir: t.TempDir(),
		assembler: assembler.New(assembler.Options{
			FileInfo: func(assembler.Owner, int) (assembler.FileInfo, error) { return assembler.FileInfo{}, nil },
		}, slog.New(slog.DiscardHandler)),
		fileInfo:    make(map[fileKey]fileEntry),
		numWorkers:  1,
		completions: completions,
	}

	// The one worker parks in registerFile's RLock on the first result, so
	// work (capacity 2) fills and run() blocks handing over the fourth.
	p.mu.Lock()
	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan struct{})
	go func() { p.run(ctx); close(ran) }()
	for range 4 {
		completions <- heldResult(t, budget, j, 0)
	}
	cancel()

	// Still holding the lock: only run()'s own drop can give bytes back.
	deadline := time.Now().Add(10 * time.Second)
	for budget.Held() != 300 {
		if time.Now().After(deadline) {
			p.mu.Unlock()
			t.Fatalf("Held() = %d after run() dropped a result at shutdown, want 300: the dropped result kept its hold", budget.Held())
		}
		time.Sleep(time.Millisecond)
	}

	p.mu.Unlock()
	<-ran
	requireNothingHeld(t, budget, "shutdown with results queued for a worker")
}
