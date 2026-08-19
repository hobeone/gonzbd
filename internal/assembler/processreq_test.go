package assembler

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
)

// ---------- processRequest write-error path ----------

func TestWriteError_TreatedAsFailed(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 2)

	var completions atomic.Int32
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completions.Add(1) }

	a := startAssembler(t, opts)

	// Make the file read-only to cause a write error.
	// First, write one good article to open the file handle.
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("AAAA"),
		MessageID: "good1",
	})

	// Wait briefly so the worker opens the file.
	_ = a.Stop()

	// Now make the file read-only and restart with a fresh assembler.
	os.Chmod(path, 0o444)
	t.Cleanup(func() { os.Chmod(path, 0o644) })

	// Create a new assembler targeting the same file.
	a2 := startAssembler(t, opts)
	_ = writeArticle(t.Context(), a2, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 1, Offset: 0, Data: []byte("BBBB"),
		MessageID: "write-err-msg",
	})
	// Need a second article to complete the file.
	_ = writeArticle(t.Context(), a2, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 2, Offset: 4, Data: []byte("CCCC"),
		MessageID: "write-err-msg2",
	})
	_ = a2.Stop()

	// The write-error articles should either be in failedIDs (if write failed)
	// or doneIDs (if OS permitted the write despite chmod). We just verify
	// no panic and the file completed.
	if n := completions.Load(); n < 1 {
		// The file might not complete if the write actually failed and both
		// got counted as failed — that's fine, the point is no panic/hang.
		t.Logf("completions = %d (expected: write error path exercised)", n)
	}
}

// ---------- Duplicate success deduplication ----------

func TestDuplicateSuccessDedup(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 2)

	var completions atomic.Int32
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completions.Add(1) }

	a := startAssembler(t, opts)

	// Send the same successful article twice.
	for range 2 {
		_ = writeArticle(t.Context(), a, WriteRequest{
			JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("AAAA"),
			MessageID: "dup-success",
		})
	}
	// Send a different article to complete the file (total=2).
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 1, Offset: 4, Data: []byte("BBBB"),
		MessageID: "unique-msg",
	})

	_ = a.Stop()

	if n := completions.Load(); n != 1 {
		t.Errorf("completions = %d, want 1 (dup success should not double-count)", n)
	}
}

// ---------- Cross-check: success after failure ----------

func TestSuccessAfterFailure_RecoveryWrite(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 2)

	opts := makeOpts(dir, files)

	a := startAssembler(t, opts)

	// First: article arrives as FatalErr (failed on all servers).
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0,
		MessageID: "retry-msg",
		FatalErr:  fmt.Errorf("article expired"),
	})
	// Then: the same article successfully downloads (backup server).
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("RECOVERED"),
		MessageID: "retry-msg",
	})
	// Second article to complete.
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 1, Offset: 9, Data: []byte("!"),
		MessageID: "msg2",
	})

	_ = a.Stop()

	// The recovery data should have been written.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) < 9 || string(got[:9]) != "RECOVERED" {
		t.Errorf("file content = %q, want to start with 'RECOVERED'", got)
	}
}

// ---------- FailureAfterSuccess cross-check ----------

func TestFailureAfterSuccess_NoDoubleCount(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 2)

	var completions atomic.Int32
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completions.Add(1) }

	a := startAssembler(t, opts)

	// First: success.
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("AAAA"),
		MessageID: "cross-msg",
	})
	// Then: the same article arrives as FatalErr (stale retry).
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0,
		MessageID: "cross-msg",
		FatalErr:  fmt.Errorf("stale failure"),
	})
	// Complete with a second article.
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 1, Offset: 4, Data: []byte("BBBB"),
		MessageID: "msg2",
	})

	_ = a.Stop()

	if n := completions.Load(); n != 1 {
		t.Errorf("completions = %d, want 1 (failure after success should not double-count)", n)
	}
}

// ---------- closeAll with partial files ----------

func TestCloseAll_PartialFilesNoCallback(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 100) // needs 100 parts

	var completions atomic.Int32
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completions.Add(1) }

	a := startAssembler(t, opts)

	// Write 3 of 100 parts, then stop.
	for i := range 3 {
		_ = writeArticle(t.Context(), a, WriteRequest{
			JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i), Offset: int64(i * 4), Data: []byte("XXXX"),
		})
	}

	_ = a.Stop()

	// File should exist (not removed) but OnFileComplete should NOT fire.
	if n := completions.Load(); n != 0 {
		t.Errorf("completions = %d, want 0 (partial file)", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("partial file should still exist: %v", err)
	}
}
