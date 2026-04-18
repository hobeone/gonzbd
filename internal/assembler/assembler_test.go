package assembler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// helper: build Options with a simple in-memory FileInfo resolver.
func makeOpts(dir string, files map[string]FileInfo) Options {
	return Options{
		FileInfo: func(jobID string, fileIdx int) (FileInfo, error) {
			key := fmt.Sprintf("%s:%d", jobID, fileIdx)
			fi, ok := files[key]
			if !ok {
				return FileInfo{}, fmt.Errorf("no FileInfo for %s", key)
			}
			return fi, nil
		},
	}
}

// helper: register a file entry in the resolver map and create the directory.
func registerFile(t *testing.T, dir string, files map[string]FileInfo, jobID string, fileIdx, totalParts int) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("%s_%d.dat", jobID, fileIdx))
	key := fmt.Sprintf("%s:%d", jobID, fileIdx)
	files[key] = FileInfo{Path: path, TotalParts: totalParts}
	return path
}

// helper: read entire file and return bytes.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readFile %s: %v", path, err)
	}
	return data
}

// startAssembler creates, starts, and registers a Stop cleanup.
func startAssembler(t *testing.T, opts Options) *Assembler {
	t.Helper()
	a := New(opts, nil)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })
	return a
}

// ---- Tests ---------------------------------------------------------------

func TestOutOfOrderAssembly(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 3)

	// Three 4-byte articles at non-sequential offsets.
	art := []struct {
		offset int64
		data   []byte
	}{
		{8, []byte("CCCC")},
		{0, []byte("AAAA")},
		{4, []byte("BBBB")},
	}

	var completions []string
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(jobID string, fileIdx int) {
		completions = append(completions, fmt.Sprintf("%s:%d", jobID, fileIdx))
	}

	a := startAssembler(t, opts)

	for _, art := range art {
		req := WriteRequest{JobID: "job1", FileIdx: 0, Offset: art.offset, Data: art.data}
		if err := a.WriteArticle(context.Background(), req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop closes workerDone; cleanup won't double-stop.

	got := readFile(t, path)
	want := []byte("AAAABBBBCCCC")
	if string(got) != string(want) {
		t.Errorf("file content = %q, want %q", got, want)
	}
	if len(completions) != 1 || completions[0] != "job1:0" {
		t.Errorf("completions = %v, want [job1:0]", completions)
	}
}

func TestFileCompleteCallbackFiresExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 3)

	var count atomic.Int32
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { count.Add(1) }

	a := startAssembler(t, opts)

	for i := range 3 {
		req := WriteRequest{JobID: "job1", FileIdx: 0, Offset: int64(i * 4), Data: []byte("XXXX")}
		if err := a.WriteArticle(context.Background(), req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if n := count.Load(); n != 1 {
		t.Errorf("OnFileComplete fired %d times, want 1", n)
	}
}

func TestMultipleFilesInterleaved(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	pathA := registerFile(t, dir, files, "job1", 0, 2)
	pathB := registerFile(t, dir, files, "job1", 1, 2)

	completed := make(map[string]bool)
	var mu sync.Mutex
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(jobID string, fileIdx int) {
		mu.Lock()
		completed[fmt.Sprintf("%s:%d", jobID, fileIdx)] = true
		mu.Unlock()
	}

	a := startAssembler(t, opts)

	reqs := []WriteRequest{
		{JobID: "job1", FileIdx: 0, Offset: 0, Data: []byte("AA")},
		{JobID: "job1", FileIdx: 1, Offset: 0, Data: []byte("XX")},
		{JobID: "job1", FileIdx: 0, Offset: 2, Data: []byte("BB")},
		{JobID: "job1", FileIdx: 1, Offset: 2, Data: []byte("YY")},
	}
	for _, r := range reqs {
		if err := a.WriteArticle(context.Background(), r); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if string(readFile(t, pathA)) != "AABB" {
		t.Errorf("file A content wrong: %q", readFile(t, pathA))
	}
	if string(readFile(t, pathB)) != "XXYY" {
		t.Errorf("file B content wrong: %q", readFile(t, pathB))
	}
	mu.Lock()
	defer mu.Unlock()
	if !completed["job1:0"] || !completed["job1:1"] {
		t.Errorf("not all files completed: %v", completed)
	}
}

func TestFileInfoError(t *testing.T) {
	// FileInfo returns an error for (job1, 0); assembler should discard the
	// write without panicking and without firing OnFileComplete.
	var completions atomic.Int32
	opts := Options{
		FileInfo: func(_ string, _ int) (FileInfo, error) {
			return FileInfo{}, fmt.Errorf("no such file")
		},
		OnFileComplete: func(_ string, _ int) { completions.Add(1) },
	}

	a := startAssembler(t, opts)

	req := WriteRequest{JobID: "job1", FileIdx: 0, Offset: 0, Data: []byte("data")}
	if err := a.WriteArticle(context.Background(), req); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if n := completions.Load(); n != 0 {
		t.Errorf("OnFileComplete fired %d times for a FileInfo-error file, want 0", n)
	}
}

func TestLowDiskCallback(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	// Use more than diskCheckInterval articles so the check triggers.
	total := diskCheckInterval + 1
	registerFile(t, dir, files, "job1", 0, total)

	var lowDiskCount atomic.Int32
	opts := makeOpts(dir, files)
	// Set MinFreeBytes to 10 PiB to guarantee the callback fires on any real disk.
	const tenPiB = 10 * (1 << 50)
	opts.MinFreeBytes = tenPiB
	opts.OnLowDisk = func(_ string, _ int64) { lowDiskCount.Add(1) }

	a := startAssembler(t, opts)

	for i := range total {
		req := WriteRequest{JobID: "job1", FileIdx: 0, Offset: int64(i * 4), Data: []byte("XXXX")}
		if err := a.WriteArticle(context.Background(), req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if n := lowDiskCount.Load(); n == 0 {
		t.Error("OnLowDisk never fired with MinFreeBytes=10PiB, want ≥1 call")
	}
}

func TestLowDiskCallbackDisabledWhenZero(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	total := diskCheckInterval + 1
	registerFile(t, dir, files, "job1", 0, total)

	var lowDiskCount atomic.Int32
	opts := makeOpts(dir, files)
	opts.MinFreeBytes = 0 // disabled
	opts.OnLowDisk = func(_ string, _ int64) { lowDiskCount.Add(1) }

	a := startAssembler(t, opts)

	for i := range total {
		req := WriteRequest{JobID: "job1", FileIdx: 0, Offset: int64(i * 4), Data: []byte("XXXX")}
		if err := a.WriteArticle(context.Background(), req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if n := lowDiskCount.Load(); n != 0 {
		t.Errorf("OnLowDisk fired %d times with MinFreeBytes=0, want 0", n)
	}
}

func TestStopDrainsChannel(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	const n = 10
	path := registerFile(t, dir, files, "job1", 0, n)

	a := New(makeOpts(dir, files), nil)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Enqueue n writes before stopping.
	for i := range n {
		req := WriteRequest{JobID: "job1", FileIdx: 0, Offset: int64(i * 4), Data: []byte("WXYZ")}
		if err := a.WriteArticle(context.Background(), req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	goroutinesBefore := runtime.NumGoroutine()

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After Stop the worker must have exited. Allow a brief settling period for
	// goroutines that are in the process of unwinding.
	time.Sleep(10 * time.Millisecond)
	goroutinesAfter := runtime.NumGoroutine()
	if goroutinesAfter > goroutinesBefore {
		t.Errorf("goroutine count increased after Stop: before=%d after=%d",
			goroutinesBefore, goroutinesAfter)
	}

	// The file must have been created (some writes landed).
	if _, err := os.Stat(path); err != nil {
		t.Errorf("target file not created after Stop+drain: %v", err)
	}
}

func TestStopBeforeStartIsSafe(t *testing.T) {
	a := New(Options{
		FileInfo: func(_ string, _ int) (FileInfo, error) { return FileInfo{}, nil },
	}, nil)
	if err := a.Stop(); err != nil {
		t.Errorf("Stop before Start returned error: %v", err)
	}
}

func TestStopCalledTwiceIsSafe(t *testing.T) {
	a := New(Options{
		FileInfo: func(_ string, _ int) (FileInfo, error) { return FileInfo{}, nil },
	}, nil)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := a.Stop(); err != nil {
		t.Errorf("second Stop returned error: %v", err)
	}
}

func TestWriteArticleAfterStopReturnsErrStopped(t *testing.T) {
	a := New(Options{
		FileInfo: func(_ string, _ int) (FileInfo, error) { return FileInfo{}, nil },
	}, nil)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	err := a.WriteArticle(context.Background(), WriteRequest{})
	if !errors.Is(err, ErrStopped) {
		t.Errorf("WriteArticle after Stop returned %v, want ErrStopped", err)
	}
}

func TestWriteArticleBeforeStartReturnsErrNotStarted(t *testing.T) {
	a := New(Options{
		FileInfo: func(_ string, _ int) (FileInfo, error) { return FileInfo{}, nil },
	}, nil)
	err := a.WriteArticle(context.Background(), WriteRequest{})
	if !errors.Is(err, ErrNotStarted) {
		t.Errorf("WriteArticle before Start returned %v, want ErrNotStarted", err)
	}
}

func TestContextCancelDuringWriteArticleSend(t *testing.T) {
	// Use queue size 1 and fill it, then cancel ctx while trying to enqueue.
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	// File with many parts so it stays open and never completes.
	registerFile(t, dir, files, "job1", 0, 1000)

	opts := makeOpts(dir, files)
	opts.QueueSize = 1

	a := New(opts, nil)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = a.Stop() }()

	// Fill the channel. The worker may drain it concurrently, so keep
	// sending until we can reliably block. We signal through a channel
	// that the worker is busy by flooding with many small articles.
	// Strategy: send enough to fill the queue, then use a slow FileInfo
	// to stall the worker, then cancel.

	// Simpler approach: use a separate assembler where the worker is blocked.
	// Actually: just fill the queue with a large burst and hope one
	// context-cancel attempt hits the full channel.
	//
	// The most reliable approach: use a FileInfo that blocks until we signal.
	blockWorker := make(chan struct{})
	unblockWorker := make(chan struct{})
	opts2 := Options{
		QueueSize: 1,
		FileInfo: func(_ string, _ int) (FileInfo, error) {
			// Block the worker on the first call until we signal.
			<-blockWorker
			<-unblockWorker
			return FileInfo{}, fmt.Errorf("intentional error to discard")
		},
	}
	a2 := New(opts2, nil)
	if err := a2.Start(context.Background()); err != nil {
		t.Fatalf("Start a2: %v", err)
	}
	defer func() { _ = a2.Stop() }()

	// Send first request; the worker will call FileInfo and block.
	go func() {
		_ = a2.WriteArticle(context.Background(), WriteRequest{JobID: "j", FileIdx: 0, Offset: 0, Data: []byte("x")})
	}()

	// Wait for the worker to enter FileInfo (channel drained, worker blocked).
	close(blockWorker)
	time.Sleep(5 * time.Millisecond)

	// Now the queue is empty but the worker is blocked in FileInfo.
	// Fill the queue (cap 1) with another request.
	fillCtx, fillCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer fillCancel()
	_ = a2.WriteArticle(fillCtx, WriteRequest{JobID: "j", FileIdx: 1, Offset: 0, Data: []byte("y")})

	// Now try to enqueue with a cancellable context — should get ctx.Err() or ErrStopped.
	cancelCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- a2.WriteArticle(cancelCtx, WriteRequest{JobID: "j", FileIdx: 2, Offset: 0, Data: []byte("z")})
	}()
	cancel()

	close(unblockWorker)

	select {
	case err := <-errCh:
		// Accept context.Canceled or ErrStopped (if Stop raced) or nil (request
		// squeezed through before cancel). Anything else is unexpected.
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrStopped) {
			t.Errorf("WriteArticle with cancelled ctx returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("WriteArticle with cancelled ctx did not return")
	}
}

func TestConcurrentWriteArticle(t *testing.T) {
	// 8 goroutines, 10 files, 10 articles each = 100 total articles.
	// Verify final file contents are correct under -race.
	const (
		numFiles     = 10
		partsPerFile = 10
		articleSize  = 8
	)

	dir := t.TempDir()
	files := make(map[string]FileInfo)

	// Pre-build expected content for each file.
	expected := make(map[string][]byte, numFiles)
	for fi := range numFiles {
		path := registerFile(t, dir, files, "job1", fi, partsPerFile)
		buf := make([]byte, partsPerFile*articleSize)
		for part := range partsPerFile {
			copy(buf[part*articleSize:], fmt.Sprintf("%04d%04d", fi, part))
		}
		expected[path] = buf
	}

	var completions atomic.Int32
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completions.Add(1) }

	a := startAssembler(t, opts)

	// Build all requests up front.
	allReqs := make([]WriteRequest, 0, numFiles*partsPerFile)
	for fi := range numFiles {
		for part := range partsPerFile {
			allReqs = append(allReqs, WriteRequest{
				JobID:   "job1",
				FileIdx: fi,
				Offset:  int64(part * articleSize),
				Data:    []byte(fmt.Sprintf("%04d%04d", fi, part)),
			})
		}
	}

	// Dispatch from 8 goroutines concurrently.
	var wg sync.WaitGroup
	const workers = 8
	chunk := len(allReqs) / workers
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			start := w * chunk
			end := start + chunk
			if w == workers-1 {
				end = len(allReqs)
			}
			for _, req := range allReqs[start:end] {
				if err := a.WriteArticle(context.Background(), req); err != nil {
					t.Errorf("WriteArticle: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if n := completions.Load(); int(n) != numFiles {
		t.Errorf("completions = %d, want %d", n, numFiles)
	}

	// Verify each file.
	for fi := range numFiles {
		path := filepath.Join(dir, fmt.Sprintf("job1_%d.dat", fi))
		got := readFile(t, path)
		want := expected[path]
		if len(got) != len(want) {
			t.Errorf("file %d: length %d, want %d", fi, len(got), len(want))
			continue
		}

		// Read each part's slot and verify it matches any valid part data
		// (order of arrival is non-deterministic but each offset is idempotent).
		fh, err := os.Open(path)
		if err != nil {
			t.Errorf("open file %d: %v", fi, err)
			continue
		}
		content, err := io.ReadAll(fh)
		fh.Close() //nolint:errcheck // read-only file, close error irrelevant
		if err != nil {
			t.Errorf("read file %d: %v", fi, err)
			continue
		}
		for part := range partsPerFile {
			partSlot := content[part*articleSize : (part+1)*articleSize]
			wantSlot := fmt.Sprintf("%04d%04d", fi, part)
			if string(partSlot) != wantSlot {
				t.Errorf("file %d part %d: got %q, want %q", fi, part, partSlot, wantSlot)
			}
		}
	}
}

func TestFreeBytes(t *testing.T) {
	dir := t.TempDir()
	free, err := FreeBytes(dir)
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}
	if free <= 0 {
		t.Errorf("FreeBytes returned %d, want > 0", free)
	}
	t.Logf("FreeBytes(%s) = %d", dir, free)
}
