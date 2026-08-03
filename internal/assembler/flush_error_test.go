package assembler

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

var errQueueGone = errors.New("queue: job not resident")

// flush's queue callbacks are best-effort by design: the bytes are already on
// disk, and a job removed or evicted between the write and the flush is
// ordinary. Since the residency work those callbacks report that case as an
// error rather than a silent nil, so flush now takes its error branches in
// normal operation rather than only on genuine failure.
//
// This pins that they stay non-fatal. Every callback fails, and the assembler
// must still drain each batch, attempt every one of them, and stop cleanly —
// a failing MarkArticlesDone must not prevent the failed batch or the write
// cursors from being attempted.
func TestFlush_QueueErrorsAreLoggedNotFatal(t *testing.T) {
	var doneCalls, failedCalls, cursorCalls atomic.Int32

	opts := Options{
		FileInfo: func(jobID string, fileIdx int) (FileInfo, error) {
			return FileInfo{Path: t.TempDir() + "/out.bin", TotalParts: 2}, nil
		},
		MarkArticlesDoneByIdx: func(jobID string, artIdxs []int32) error {
			doneCalls.Add(1)
			return errQueueGone
		},
		MarkArticlesFailedByIdx: func(jobID string, artIdxs []int32) ([]int32, error) {
			failedCalls.Add(1)
			return nil, errQueueGone
		},
		SetWriteCursor: func(jobID string, fileIdx int, cursor int64) error {
			cursorCalls.Add(1)
			return errQueueGone
		},
		DoneFlushInterval: 10 * time.Millisecond,
	}

	a := startAssembler(t, opts)
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "j1", FileIdx: 0, ArtIdx: 0, MessageID: "msg0", Data: []byte("A"),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "j1", FileIdx: 0, ArtIdx: 1, MessageID: "msg1",
		FatalErr: errors.New("article unavailable on all servers"),
	}); err != nil {
		t.Fatalf("WriteArticle (fatal): %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop returned %v; a failing queue callback must not make shutdown fail", err)
	}

	if doneCalls.Load() == 0 {
		t.Error("MarkArticlesDoneByIdx was never called")
	}
	if failedCalls.Load() == 0 {
		t.Error("MarkArticlesFailedByIdx was never called; an error from the done batch must not skip the failed batch")
	}
	// SetWriteCursor is wired so that if a cursor is pending its error
	// branch is exercised too, but it is deliberately not asserted on: a
	// cursor is only recorded when write caching buffers a contiguous run
	// for a file that does not then complete, which is a different fixture
	// than this one and not what this test is about.
	_ = cursorCalls.Load()
}

// The same, through the string-keyed fallbacks. flush picks these only when
// the ByIdx callbacks are nil, so their error branches are a separate pair of
// lines that the ByIdx test above cannot reach.
func TestFlush_QueueErrorsAreLoggedNotFatal_StringFallback(t *testing.T) {
	var doneCalls, failedCalls atomic.Int32

	opts := Options{
		FileInfo: func(jobID string, fileIdx int) (FileInfo, error) {
			return FileInfo{Path: t.TempDir() + "/out.bin", TotalParts: 2}, nil
		},
		MarkArticlesDoneByIdx:   nil,
		MarkArticlesFailedByIdx: nil,
		MarkArticlesDone: func(jobID string, msgIDs []string) error {
			doneCalls.Add(1)
			return errQueueGone
		},
		MarkArticlesFailed: func(jobID string, msgIDs []string) ([]string, error) {
			failedCalls.Add(1)
			return nil, errQueueGone
		},
		DoneFlushInterval: 10 * time.Millisecond,
	}

	a := startAssembler(t, opts)
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "j1", FileIdx: 0, ArtIdx: 0, MessageID: "msg0", Data: []byte("A"),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "j1", FileIdx: 0, ArtIdx: 1, MessageID: "msg1",
		FatalErr: errors.New("article unavailable on all servers"),
	}); err != nil {
		t.Fatalf("WriteArticle (fatal): %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop returned %v; a failing queue callback must not make shutdown fail", err)
	}

	if doneCalls.Load() == 0 {
		t.Error("MarkArticlesDone fallback was never called")
	}
	if failedCalls.Load() == 0 {
		t.Error("MarkArticlesFailed fallback was never called")
	}
}
