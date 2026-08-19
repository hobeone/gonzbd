package assembler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- CancelJob ----------

func TestCancelJob_ClosesOpenFileHandles(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	pathA := registerFile(t, dir, files, "job1", 0, 10)
	pathB := registerFile(t, dir, files, "job1", 1, 10)

	// Use a FileInfo resolver that signals when it's been called, so we
	// know the worker has opened the files before we send the cancel.
	var fileInfoCalls atomic.Int32
	opts := Options{
		FileInfo: func(jobID string, fileIdx int) (FileInfo, error) {
			key := fmt.Sprintf("%s:%d", jobID, fileIdx)
			fi, ok := files[key]
			if !ok {
				return FileInfo{}, fmt.Errorf("no FileInfo for %s", key)
			}
			fileInfoCalls.Add(1)
			return fi, nil
		},
	}
	a := startAssembler(t, opts)

	// Write one article per file to open the file handles.
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("hello"),
	})
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 1, ArtIdx: 1, Offset: 0, Data: []byte("world"),
	})

	// Wait for the worker to have processed both file opens.
	deadline := time.After(2 * time.Second)
	for fileInfoCalls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for FileInfo calls")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// Cancel the job — both files should be closed and removed.
	if err := a.CancelJob(t.Context(), "job1"); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}

	// Stop to drain and flush.
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Both files should have been removed by CancelJob.
	if _, err := os.Stat(pathA); !os.IsNotExist(err) {
		t.Errorf("file A should have been removed, err = %v", err)
	}
	if _, err := os.Stat(pathB); !os.IsNotExist(err) {
		t.Errorf("file B should have been removed, err = %v", err)
	}
}

func TestCancelJob_RejectsLateArticles(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 10)

	var completions atomic.Int32
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completions.Add(1) }

	a := startAssembler(t, opts)

	// Write one article then cancel.
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("data"),
	})
	_ = a.CancelJob(t.Context(), "job1")

	// Send more articles after cancellation — they should be silently dropped.
	for i := 1; i < 5; i++ {
		_ = writeArticle(t.Context(), a, WriteRequest{
			JobID: "job1", FileIdx: 0, ArtIdx: int32(i + 1), Offset: int64(i * 4), Data: []byte("late"), //nolint:gosec // G115: loop bound is small
		})
	}

	_ = a.Stop()

	if n := completions.Load(); n != 0 {
		t.Errorf("OnFileComplete fired %d times for cancelled job, want 0", n)
	}
}

func TestCancelJob_DoesNotAffectOtherJobs(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 2)
	path2 := registerFile(t, dir, files, "job2", 0, 2)

	var completed sync.Map
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(jobID string, fileIdx int) {
		completed.Store(fmt.Sprintf("%s:%d", jobID, fileIdx), true)
	}

	a := startAssembler(t, opts)

	// Write to both jobs.
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("AAAA"),
	})
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job2", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("XXXX"),
	})

	// Cancel only job1.
	_ = a.CancelJob(t.Context(), "job1")

	// Finish job2.
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job2", FileIdx: 0, ArtIdx: 1, Offset: 4, Data: []byte("YYYY"),
	})

	_ = a.Stop()

	// Job2 should have completed.
	if _, ok := completed.Load("job2:0"); !ok {
		t.Error("job2 should have completed")
	}
	// Job2 file should exist with correct content.
	got := readFile(t, path2)
	if string(got) != "XXXXYYYY" {
		t.Errorf("job2 content = %q, want %q", got, "XXXXYYYY")
	}
}

func TestCancelJob_BeforeStartReturnsError(t *testing.T) {
	a := New(Options{
		FileInfo: func(_ string, _ int) (FileInfo, error) { return FileInfo{}, nil },
	}, nil)
	err := a.CancelJob(t.Context(), "job1")
	if !errors.Is(err, ErrNotStarted) {
		t.Errorf("CancelJob before Start = %v, want ErrNotStarted", err)
	}
}

func TestCancelJob_AfterStopReturnsError(t *testing.T) {
	a := New(Options{
		FileInfo: func(_ string, _ int) (FileInfo, error) { return FileInfo{}, nil },
	}, nil)
	_ = a.Start(t.Context())
	_ = a.Stop()
	err := a.CancelJob(t.Context(), "job1")
	if !errors.Is(err, ErrStopped) {
		t.Errorf("CancelJob after Stop = %v, want ErrStopped", err)
	}
}

func TestCancelJob_ContextCancel(t *testing.T) {
	// Create an assembler where the worker is blocked.
	blockCh := make(chan struct{})
	entered := make(chan struct{}, 1)
	opts := Options{
		QueueSize: 1,
		FileInfo: func(_ string, _ int) (FileInfo, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-blockCh
			return FileInfo{}, fmt.Errorf("blocked")
		},
	}
	a := New(opts, nil)
	_ = a.Start(t.Context())
	defer func() {
		close(blockCh)
		_ = a.Stop()
	}()

	// Fill the queue with a request that blocks the worker.
	go func() {
		_ = writeArticle(t.Context(), a, WriteRequest{
			JobID: "block", FileIdx: 0, ArtIdx: 0, Data: []byte("x"),
		})
	}()
	// Deterministically wait until the worker has dequeued the request and
	// entered FileInfo (queue now drained), instead of sleeping.
	<-entered

	// Fill the queue (cap 1).
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_ = writeArticle(ctx, a, WriteRequest{
		JobID: "fill", FileIdx: 0, ArtIdx: 0, Data: []byte("y"),
	})

	// Now CancelJob with an already-cancelled context.
	cancelledCtx, cancel2 := context.WithCancel(t.Context())
	cancel2()
	err := a.CancelJob(cancelledCtx, "some-job")
	if err == nil {
		// Might have succeeded if channel had room — that's okay.
		return
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrStopped) {
		t.Errorf("CancelJob with cancelled ctx = %v, want Canceled or Stopped", err)
	}
}

// ---------- FatalErr (failed article) handling ----------

// TestFatalErrCountsTowardCompletion is trimmed from its original shape: it
// used to also assert MarkArticlesFailed received the fatal article's
// Message-ID, but the assembler no longer has any ack authority (X2) — a
// permanent failure is the queue's to record via AckPermanentFailure, not
// this package's. What is still live, and still worth pinning, is
// handleFatalArticle's local bookkeeping: a FatalErr article counts toward
// TotalParts exactly like a written one, so the file still completes.
func TestFatalErrCountsTowardCompletion(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 3)

	var completions atomic.Int32
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completions.Add(1) }

	a := startAssembler(t, opts)

	// 2 good articles, 1 fatal.
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("AAAA"),
		MessageID: "good1",
	})
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 1, Offset: 4, Data: []byte("BBBB"),
		MessageID: "good2",
	})
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 2,
		MessageID: "fail1",
		FatalErr:  fmt.Errorf("article not found on any server"),
	})

	_ = a.Stop()

	if n := completions.Load(); n != 1 {
		t.Errorf("OnFileComplete fired %d times, want 1 (fatal err counts as part)", n)
	}
}

func TestFatalErrDuplicate(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 2)

	var completions atomic.Int32
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completions.Add(1) }

	a := startAssembler(t, opts)

	// Send the same FatalErr twice — only one should count.
	for range 2 {
		_ = writeArticle(t.Context(), a, WriteRequest{
			JobID: "job1", FileIdx: 0, ArtIdx: 0,
			MessageID: "dup-fail",
			FatalErr:  fmt.Errorf("gone"),
		})
	}
	// Send one good article to complete the file (total=2 parts).
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 1, Offset: 0, Data: []byte("AAAA"),
		MessageID: "good1",
	})

	_ = a.Stop()

	if n := completions.Load(); n != 1 {
		t.Errorf("completions = %d, want 1 (dup fatal should not double-count)", n)
	}
}

// ---------- Late duplicate for completed file ----------

func TestLateDuplicateForCompletedFile(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 2)

	var completions atomic.Int32
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completions.Add(1) }

	a := startAssembler(t, opts)

	// Complete the file.
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("AA"),
		MessageID: "msg1",
	})
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 1, Offset: 2, Data: []byte("BB"),
		MessageID: "msg2",
	})

	// Send a late duplicate — should be rejected (tombstone set).
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 2, Offset: 0, Data: []byte("XX"),
		MessageID: "msg1-late",
	})

	_ = a.Stop()

	if n := completions.Load(); n != 1 {
		t.Errorf("completions = %d, want exactly 1 (no re-open on late dup)", n)
	}
}

// ---------- Double-start returns error ----------

func TestDoubleStartReturnsError(t *testing.T) {
	a := New(Options{
		FileInfo: func(_ string, _ int) (FileInfo, error) { return FileInfo{}, nil },
	}, nil)
	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer func() { _ = a.Stop() }()
	if err := a.Start(t.Context()); err == nil {
		t.Error("second Start should return an error")
	}
}

// ---------- New panics on nil FileInfo ----------

func TestNewPanicsOnNilFileInfo(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("New with nil FileInfo should panic")
		}
	}()
	New(Options{}, nil)
}
