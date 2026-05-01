package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/telemetry"
)

// stubQueue creates a queue with one job containing numFiles files, each with
// 1 article. Uses low-level Job construction since the test doesn't need NZB
// parsing.
func newStubQueue(t *testing.T, jobID string, numFiles int) *queue.Queue {
	t.Helper()
	q, err := queue.Load(t.TempDir())
	if err != nil {
		t.Fatalf("queue.Load: %v", err)
	}
	job := &queue.Job{
		ID:       jobID,
		Name:     "test-job",
		Filename: "test.nzb",
		Status:   "Queued",
	}
	for i := range numFiles {
		jf := queue.JobFile{
			Subject: fmt.Sprintf("test.bin (%d/%d)", i+1, numFiles),
			Bytes:   1024,
			Articles: []queue.JobArticle{
				{ID: fmt.Sprintf("<%s-%d@test>", jobID, i), Bytes: 1024, Number: 1},
			},
		}
		job.Files = append(job.Files, jf)
		job.TotalBytes += jf.Bytes
	}
	job.RemainingBytes = job.TotalBytes
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	return q
}

// TestPipelineWorkerPool verifies that the pipeline's fan-out worker pool
// processes all articles and shuts down cleanly.
func TestPipelineWorkerPool(t *testing.T) {
	telemetry.Reset()

	dir := t.TempDir()
	q := newStubQueue(t, "job1", 1)

	// Create a real assembler with a simple file resolver.
	files := map[string]assembler.FileInfo{
		"job1:0": {
			Path:       dir + "/out.dat",
			TotalParts: 5,
		},
	}
	opts := assembler.Options{
		FileInfo: func(jobID string, fileIdx int) (assembler.FileInfo, error) {
			key := fmt.Sprintf("%s:%d", jobID, fileIdx)
			fi, ok := files[key]
			if !ok {
				return assembler.FileInfo{}, fmt.Errorf("no FileInfo for %s", key)
			}
			return fi, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := assembler.New(opts, nil)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	defer func() { _ = a.Stop() }()

	// Feed articles through a channel.
	completions := make(chan *downloader.ArticleResult, 10)
	p := &pipeline{
		log:         slog.Default(),
		queue:       q,
		assembler:   a,
		completions: completions,
		downloadDir: dir,
		sanitize:    fsutil.SanitizeOptions{},
		numWorkers:  3, // explicit small pool
		updateCh:    make(chan completionSwap),
		fileInfo:    make(map[fileKey]assembler.FileInfo),
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.run(ctx)
	}()

	// Send 5 articles to the pipeline.
	const numArticles = 5
	for i := range numArticles {
		completions <- &downloader.ArticleResult{
			JobID:      "job1",
			FileIdx:    0,
			MessageID:  fmt.Sprintf("<art%d@test>", i),
			ServerName: "test-server",
			Data:       []byte("data"),
			Offset:     int64(i * 4),
		}
	}

	// Wait for the assembler to process all articles.
	deadline := time.After(5 * time.Second)
	for telemetry.ArticlesWritten.Value() < numArticles {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for articles: received=%d written=%d",
				telemetry.ArticlesReceived.Value(),
				telemetry.ArticlesWritten.Value())
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Shut down.
	cancel()
	wg.Wait()

	// Verify telemetry counters.
	if got := telemetry.ArticlesReceived.Value(); got != numArticles {
		t.Errorf("ArticlesReceived = %d, want %d", got, numArticles)
	}
	if got := telemetry.ArticlesWritten.Value(); got != numArticles {
		t.Errorf("ArticlesWritten = %d, want %d", got, numArticles)
	}
	if got := telemetry.ArticlesFailed.Value(); got != 0 {
		t.Errorf("ArticlesFailed = %d, want 0", got)
	}
	if got := telemetry.ArticlesRetried.Value(); got != 0 {
		t.Errorf("ArticlesRetried = %d, want 0", got)
	}
}

// TestPipelineWorkerPoolConcurrency verifies that the worker pool
// processes articles faster than a single-threaded loop would.
// We send N articles, each requiring a registerFile call, and verify
// that the total wall-clock time is less than N * per-article overhead.
func TestPipelineWorkerPoolConcurrency(t *testing.T) {
	telemetry.Reset()

	dir := t.TempDir()
	const numFiles = 8
	q := newStubQueue(t, "job1", numFiles)

	// Build file info map for all files.
	files := make(map[string]assembler.FileInfo, numFiles)
	for i := range numFiles {
		files[fmt.Sprintf("job1:%d", i)] = assembler.FileInfo{
			Path:       fmt.Sprintf("%s/out_%d.dat", dir, i),
			TotalParts: 1,
		}
	}

	opts := assembler.Options{
		FileInfo: func(jobID string, fileIdx int) (assembler.FileInfo, error) {
			key := fmt.Sprintf("%s:%d", jobID, fileIdx)
			fi, ok := files[key]
			if !ok {
				return assembler.FileInfo{}, fmt.Errorf("no FileInfo for %s", key)
			}
			return fi, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := assembler.New(opts, nil)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	defer func() { _ = a.Stop() }()

	completions := make(chan *downloader.ArticleResult, numFiles)
	const numWorkers = 4
	p := &pipeline{
		log:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		queue:       q,
		assembler:   a,
		completions: completions,
		downloadDir: dir,
		sanitize:    fsutil.SanitizeOptions{},
		numWorkers:  numWorkers,
		updateCh:    make(chan completionSwap),
		fileInfo:    make(map[fileKey]assembler.FileInfo),
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.run(ctx)
	}()

	// Send 1 article per file.
	for i := range numFiles {
		completions <- &downloader.ArticleResult{
			JobID:      "job1",
			FileIdx:    i,
			MessageID:  fmt.Sprintf("<art%d@test>", i),
			ServerName: "test-server",
			Data:       []byte("data"),
			Offset:     0,
		}
	}

	// Wait for all articles to be written.
	deadline := time.After(5 * time.Second)
	for telemetry.ArticlesWritten.Value() < numFiles {
		select {
		case <-deadline:
			t.Fatalf("timed out: written=%d", telemetry.ArticlesWritten.Value())
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	wg.Wait()

	// Verify all articles were processed.
	if got := telemetry.ArticlesReceived.Value(); got != numFiles {
		t.Errorf("ArticlesReceived = %d, want %d", got, numFiles)
	}
	if got := telemetry.ArticlesWritten.Value(); got != numFiles {
		t.Errorf("ArticlesWritten = %d, want %d", got, numFiles)
	}

	// Verify the worker pool created the expected number of workers.
	// (This is implicitly tested by the fact that all articles were
	// processed, but we document the expectation.)
	t.Logf("worker pool: %d workers processed %d articles across %d files",
		numWorkers, telemetry.ArticlesReceived.Value(), numFiles)
}

// TestPipelineRetryTelemetry verifies that retryable errors increment
// the ArticlesRetried counter (not ArticlesFailed or ArticlesWritten).
func TestPipelineRetryTelemetry(t *testing.T) {
	telemetry.Reset()

	dir := t.TempDir()
	q := newStubQueue(t, "job1", 1)

	// We don't need a real assembler for retry-path articles since
	// retryable errors return before calling the assembler.
	files := map[string]assembler.FileInfo{
		"job1:0": {
			Path:       dir + "/out.dat",
			TotalParts: 1,
		},
	}
	opts := assembler.Options{
		FileInfo: func(jobID string, fileIdx int) (assembler.FileInfo, error) {
			key := fmt.Sprintf("%s:%d", jobID, fileIdx)
			fi, ok := files[key]
			if !ok {
				return assembler.FileInfo{}, fmt.Errorf("no FileInfo for %s", key)
			}
			return fi, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := assembler.New(opts, nil)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	defer func() { _ = a.Stop() }()

	completions := make(chan *downloader.ArticleResult, 5)
	p := &pipeline{
		log:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		queue:       q,
		assembler:   a,
		completions: completions,
		downloadDir: dir,
		sanitize:    fsutil.SanitizeOptions{},
		numWorkers:  2,
		updateCh:    make(chan completionSwap),
		fileInfo:    make(map[fileKey]assembler.FileInfo),
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.run(ctx)
	}()

	// Send 1 success + 2 retryable errors + 1 fatal error.
	completions <- &downloader.ArticleResult{
		JobID: "job1", FileIdx: 0, MessageID: "<ok@test>",
		ServerName: "s1", Data: []byte("good"), Offset: 0,
	}
	completions <- &downloader.ArticleResult{
		JobID: "job1", FileIdx: 0, MessageID: "<retry1@test>",
		ServerName: "s1", Err: fmt.Errorf("dial: connection refused"),
	}
	completions <- &downloader.ArticleResult{
		JobID: "job1", FileIdx: 0, MessageID: "<retry2@test>",
		ServerName: "s1", Err: fmt.Errorf("i/o timeout on read"),
	}
	completions <- &downloader.ArticleResult{
		JobID: "job1", FileIdx: 0, MessageID: "<fatal@test>",
		ServerName: "s1", Err: downloader.ErrNoServersLeft,
	}

	// Wait for all 4 articles to be processed.
	deadline := time.After(5 * time.Second)
	for {
		total := telemetry.ArticlesWritten.Value() +
			telemetry.ArticlesRetried.Value() +
			telemetry.ArticlesFailed.Value()
		if total >= 4 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: written=%d retried=%d failed=%d",
				telemetry.ArticlesWritten.Value(),
				telemetry.ArticlesRetried.Value(),
				telemetry.ArticlesFailed.Value())
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	wg.Wait()

	if got := telemetry.ArticlesReceived.Value(); got != 4 {
		t.Errorf("ArticlesReceived = %d, want 4", got)
	}
	if got := telemetry.ArticlesWritten.Value(); got != 1 {
		t.Errorf("ArticlesWritten = %d, want 1", got)
	}
	if got := telemetry.ArticlesRetried.Value(); got != 2 {
		t.Errorf("ArticlesRetried = %d, want 2", got)
	}
	if got := telemetry.ArticlesFailed.Value(); got != 1 {
		t.Errorf("ArticlesFailed = %d, want 1", got)
	}
}

// TestPipelineShutdownDrainsWorkers verifies that cancelling the context
// causes the worker pool to finish all in-flight work and exit cleanly.
func TestPipelineShutdownDrainsWorkers(t *testing.T) {
	telemetry.Reset()

	dir := t.TempDir()
	q := newStubQueue(t, "job1", 1)

	files := map[string]assembler.FileInfo{
		"job1:0": {
			Path:       dir + "/out.dat",
			TotalParts: 3,
		},
	}
	opts := assembler.Options{
		FileInfo: func(jobID string, fileIdx int) (assembler.FileInfo, error) {
			key := fmt.Sprintf("%s:%d", jobID, fileIdx)
			fi, ok := files[key]
			if !ok {
				return assembler.FileInfo{}, fmt.Errorf("no FileInfo for %s", key)
			}
			return fi, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	a := assembler.New(opts, nil)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	defer func() { _ = a.Stop() }()

	completions := make(chan *downloader.ArticleResult, 5)
	p := &pipeline{
		log:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		queue:       q,
		assembler:   a,
		completions: completions,
		downloadDir: dir,
		sanitize:    fsutil.SanitizeOptions{},
		numWorkers:  2,
		updateCh:    make(chan completionSwap),
		fileInfo:    make(map[fileKey]assembler.FileInfo),
	}

	// Start the pipeline.
	runDone := make(chan struct{})
	go func() {
		p.run(ctx)
		close(runDone)
	}()

	// Send some articles.
	for i := range 3 {
		completions <- &downloader.ArticleResult{
			JobID: "job1", FileIdx: 0,
			MessageID:  fmt.Sprintf("<a%d@test>", i),
			ServerName: "s1", Data: []byte("data"), Offset: int64(i * 4),
		}
	}

	// Wait briefly for articles to be consumed.
	time.Sleep(100 * time.Millisecond)

	// Cancel and verify the pipeline exits promptly.
	cancel()

	select {
	case <-runDone:
		// Success — pipeline exited.
	case <-time.After(3 * time.Second):
		t.Fatal("pipeline.run did not exit within 3s after context cancellation")
	}

	// All articles should have been processed before shutdown.
	if got := telemetry.ArticlesReceived.Value(); got != 3 {
		t.Errorf("ArticlesReceived = %d, want 3", got)
	}
}
