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

// TestRegisterFile_ResumedFile pins the signal finalizeFile's interim
// truncation guard depends on. The assembler cannot derive it: a resumed file
// and a fresh one look identical from inside, and getting this wrong in either
// direction is a data defect — false suppresses the guard and a resumed file
// is truncated to this run's extent (#342/#350), true suppresses the truncate
// on every fresh download and leaves pre-allocation's zeros for par2 to report
// as damage.
func TestRegisterFile_ResumedFile(t *testing.T) {
	t.Run("false when no article is already done", func(t *testing.T) {
		q, job := helperJob(t, "fresh", 1, 4)
		p := helperPipeline(t, q)
		if err := p.registerFile(job.ID, 0); err != nil {
			t.Fatalf("registerFile: %v", err)
		}
		info, err := p.resolveFileInfo(job.ID, 0)
		if err != nil {
			t.Fatalf("resolveFileInfo: %v", err)
		}
		if info.ResumedFile {
			t.Error("ResumedFile = true for a file with every article outstanding")
		}
		if info.TotalParts != 4 {
			t.Errorf("TotalParts = %d, want 4", info.TotalParts)
		}
	})

	t.Run("true when some articles are already done", func(t *testing.T) {
		q, job := helperJob(t, "resumed", 1, 4)
		if err := q.MarkArticlesDone(job.ID, []string{"resumed-f0-a0@x", "resumed-f0-a1@x"}); err != nil {
			t.Fatalf("MarkArticlesDone: %v", err)
		}
		p := helperPipeline(t, q)
		if err := p.registerFile(job.ID, 0); err != nil {
			t.Fatalf("registerFile: %v", err)
		}
		info, err := p.resolveFileInfo(job.ID, 0)
		if err != nil {
			t.Fatalf("resolveFileInfo: %v", err)
		}
		if !info.ResumedFile {
			t.Error("ResumedFile = false for a file whose earlier run completed two articles — " +
				"finalizeFile will truncate away what that run wrote (#342/#350)")
		}
		if info.TotalParts != 2 {
			t.Errorf("TotalParts = %d, want 2 (only the outstanding articles)", info.TotalParts)
		}
	})

	t.Run("is decided per file, not per job", func(t *testing.T) {
		// The count is compared against the file's own article range. A job
		// comparison would mark every file of a partly-downloaded job as
		// resumed, including files not yet touched.
		q, job := helperJob(t, "mixed", 2, 2)
		if err := q.MarkArticlesDone(job.ID, []string{"mixed-f0-a0@x"}); err != nil {
			t.Fatalf("MarkArticlesDone: %v", err)
		}
		p := helperPipeline(t, q)
		for fi := range 2 {
			if err := p.registerFile(job.ID, fi); err != nil {
				t.Fatalf("registerFile %d: %v", fi, err)
			}
		}
		f0, _ := p.resolveFileInfo(job.ID, 0)
		f1, _ := p.resolveFileInfo(job.ID, 1)
		if !f0.ResumedFile {
			t.Error("file 0 has a done article but ResumedFile = false")
		}
		if f1.ResumedFile {
			t.Error("file 1 is untouched but ResumedFile = true")
		}
	})
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

func TestHandleResult_RoutesOnError(t *testing.T) {
	// handleResult's whole job is the split, plus the heartbeat. Routing a
	// failure down the success path would hand the assembler a nil body as
	// though it were content.
	q, job := helperJob(t, "route", 1, 2)
	p := helperPipeline(t, q)
	p.assembler = assembler.New(assembler.Options{
		FileInfo: p.resolveFileInfo,
	}, nil)

	var beats int
	p.onHeartbeat = func() { beats++ }

	// A failure with no servers left is terminal, so handleFailureResult
	// reaches registerFile — observable without starting the assembler.
	p.handleResult(t.Context(), &downloader.ArticleResult{
		JobID:     job.ID,
		FileIdx:   0,
		MessageID: "route-f0-a0@x",
		Err:       downloader.ErrNoServersLeft,
		Subject:   "file0.rar",
	})
	if beats != 1 {
		t.Errorf("onHeartbeat fired %d times, want 1", beats)
	}
	if _, err := p.resolveFileInfo(job.ID, 0); err != nil {
		t.Errorf("a terminal failure did not reach the failure path: %v", err)
	}

	t.Run("fires the heartbeat for a success too", func(t *testing.T) {
		// Watchdog liveness must not depend on articles succeeding; a job
		// failing every article is still making progress.
		before := beats
		p.handleResult(t.Context(), &downloader.ArticleResult{
			JobID:     job.ID,
			FileIdx:   0,
			MessageID: "route-f0-a1@x",
			Subject:   "file0.rar",
		})
		if beats != before+1 {
			t.Errorf("onHeartbeat fired %d times, want %d", beats, before+1)
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
