package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/job"
)

// The resolver is the assembler's one gate on a closed job: a write for a job
// that has left the dispatcher, or been handed to post-processing, must not
// open a file. The register guard keeps a late article from re-growing
// fileInfo once the job is gone.

func TestResolveFileInfo_RefusesAJobThatLeftTheDispatcher(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	disp, j := helperJob(t, application, "closed-left", 1, 1)
	p := helperPipeline(t, disp)
	if err := p.registerFile(j, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	if err := disp.RemoveJob(context.Background(), j); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	_, err := p.resolveForWrite(j.ID(), 0)
	if !errors.Is(err, assembler.ErrJobClosed) {
		t.Fatalf("resolveForWrite after RemoveJob = %v, want ErrJobClosed: the assembler would open a file for a job in history", err)
	}
}

func TestResolveFileInfo_RefusesAHandedOffJob(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	disp, j := helperJob(t, application, "closed-handed", 1, 1)
	p := helperPipeline(t, disp)
	var handed atomic.Bool
	p.handedOff = func(got *job.Job) bool { return got == j && handed.Load() }
	if err := p.registerFile(j, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	if _, err := p.resolveForWrite(j.ID(), 0); err != nil {
		t.Fatalf("resolveForWrite before the hand-over = %v, want nil", err)
	}
	handed.Store(true)
	_, err := p.resolveForWrite(j.ID(), 0)
	if !errors.Is(err, assembler.ErrJobClosed) {
		t.Fatalf("resolveForWrite after the hand-over = %v, want ErrJobClosed", err)
	}
}

// TestResolveFileInfo_RefusesAnEarlierInstance: the entry carries the
// instance that registered it, so a stale write from attempt 1 arriving after
// attempt 2 registered under the same ID is refused rather than landing in
// attempt 2's file.
func TestResolveFileInfo_RefusesAnEarlierInstance(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	first, _ := retriedUnderOneID(t, application, "feedface00105301")
	p := helperPipeline(t, application.Dispatcher())
	p.mu.Lock()
	p.fileInfo[fileKey{jobID: first.ID(), fileIdx: 0}] = fileEntry{
		info: assembler.FileInfo{Dir: t.TempDir(), Name: "f0", TotalParts: 1}, j: first,
	}
	p.mu.Unlock()
	_, err := p.resolveForWrite(first.ID(), 0)
	if !errors.Is(err, assembler.ErrJobClosed) {
		t.Fatalf("resolveForWrite for an earlier instance = %v, want ErrJobClosed: attempt 1's write would land in attempt 2's file", err)
	}
}

// TestRegisterFile_RefusesOnceTheJobLeftTheDispatcher drives the window the
// issue describes: the handler's instance check has passed and the file has
// been counted, then the job leaves the dispatcher before the cache write.
// countedHook rather than nameReadHook, because RemoveJob evicts the
// manifest and a count run after the eviction fails before reaching the
// guard this test pins.
func TestRegisterFile_RefusesOnceTheJobLeftTheDispatcher(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	disp, j := helperJob(t, application, "closed-reg", 1, 1)
	p := helperPipeline(t, disp)
	inHook, release := make(chan struct{}), make(chan struct{})
	p.countedHook = func(*job.Job) { close(inHook); <-release }
	errCh := make(chan error, 1)
	go func() { errCh <- p.registerFile(j, 0) }()
	<-inHook
	if err := disp.RemoveJob(context.Background(), j); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	close(release)
	if err := <-errCh; !errors.Is(err, assembler.ErrJobClosed) {
		t.Fatalf("registerFile that counted the file before RemoveJob = %v, want ErrJobClosed", err)
	}
	p.mu.RLock()
	n := len(p.fileInfo)
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("fileInfo holds %d entries for a job that left the dispatcher, want 0", n)
	}
}

// TestHandleSuccessResult_DropsAClosedJobsArticleWithoutReturningIt: a
// register refusal for a closed job is the instance-check drop, not a
// registration failure. The article is not returned to the dispatch pool.
func TestHandleSuccessResult_DropsAClosedJobsArticleWithoutReturningIt(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	disp, j := helperJob(t, application, "closed-drop", 1, 1)
	if err := j.MarkArticleEmitted(0); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	p := helperPipeline(t, disp)
	var logs bytes.Buffer
	p.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p.assembler = assembler.New(assembler.Options{
		FileInfo: func(string, int) (assembler.FileInfo, error) {
			t.Error("WriteArticle reached the assembler for a closed job")
			return assembler.FileInfo{}, assembler.ErrJobClosed
		},
	}, slog.New(slog.DiscardHandler))
	inHook, release := make(chan struct{}), make(chan struct{})
	p.countedHook = func(*job.Job) { close(inHook); <-release }
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleResult(t.Context(), &downloader.ArticleResult{
			Job: j, FileIdx: 0, ArtIdx: 0, MessageID: "closed-drop-f0-a0@x",
			Data: []byte("payload"), ServerName: "s1",
		})
	}()
	<-inHook
	if err := disp.RemoveJob(context.Background(), j); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	close(release)
	<-done
	// The Emitted bit cannot be read back once RemoveJob has evicted the
	// manifest, so the observation is the handler's own account of which
	// path it took: the register-failure path warns and returns the article
	// to the pool, the instance-check drop says so at Debug.
	if got := logs.String(); strings.Contains(got, "returning to dispatch pool") || !strings.Contains(got, "left the dispatcher while its file was being registered") {
		t.Errorf("the article was treated as a registration failure and returned to the dispatch pool for a job that left the dispatcher; the instance-check drop leaves it Emitted. log:\n%s", got)
	}
}

// TestNew_WiresTheHandOverIntoTheResolver goes through New rather than a
// test-assigned hook: the admission half of resolveForWrite is a field the
// pipeline literal has to set, and a literal that omits it compiles, passes
// every hook-injecting test, and leaves a handed-off job's late article free
// to open a file under the post-processor.
func TestNew_WiresTheHandOverIntoTheResolver(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	_, j := helperJob(t, application, "wired-handoff", 1, 1)
	p := application.pipeline
	if err := p.registerFile(j, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	if _, err := p.resolveForWrite(j.ID(), 0); err != nil {
		t.Fatalf("resolveForWrite before the hand-over = %v, want nil", err)
	}
	if out := application.postProcAdmissions.admit(j, ""); out != admitted {
		t.Fatalf("admit = %v, want admitted", out)
	}
	_, err := p.resolveForWrite(j.ID(), 0)
	if !errors.Is(err, assembler.ErrJobClosed) {
		t.Fatalf("resolveForWrite after the hand-over = %v, want ErrJobClosed: New did not wire postProcAdmissions.has into the pipeline", err)
	}
}

// TestRegisterFile_ReplacesAnEarlierInstancesEntry: a cache entry an earlier
// attempt left under the same key must not short-circuit the retry's
// registration, or resolveForWrite refuses every write of the retry.
func TestRegisterFile_ReplacesAnEarlierInstancesEntry(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	first, second := retriedUnderOneID(t, application, "feedface00105302")
	p := helperPipeline(t, application.Dispatcher())
	p.mu.Lock()
	p.fileInfo[fileKey{jobID: first.ID(), fileIdx: 0}] = fileEntry{
		info: assembler.FileInfo{Dir: t.TempDir(), Name: "f0", TotalParts: 1}, j: first,
	}
	p.mu.Unlock()
	if err := p.registerFile(second, 0); err != nil {
		t.Fatalf("registerFile for the retry = %v", err)
	}
	if _, err := p.resolveForWrite(second.ID(), 0); err != nil {
		t.Fatalf("resolveForWrite for the retry after it registered = %v, want nil: the earlier attempt's entry still answers for the key", err)
	}
}
