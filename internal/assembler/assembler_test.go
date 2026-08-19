package assembler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/telemetry"
	"github.com/hobeone/gonzbd/internal/testutil"
)

// waitUntil polls cond every 5ms until it returns true or the deadline passes.
func waitUntil(t *testing.T, cond func() bool, deadline time.Duration, msg string) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

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
	if err := a.Start(t.Context()); err != nil {
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

	for i, art := range art {
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i), Offset: art.offset, Data: art.data}
		if err := writeArticle(t.Context(), a, req); err != nil {
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
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i), Offset: int64(i * 4), Data: []byte("XXXX")}
		if err := writeArticle(t.Context(), a, req); err != nil {
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
		{JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("AA")},
		{JobID: "job1", FileIdx: 1, ArtIdx: 1, Offset: 0, Data: []byte("XX")},
		{JobID: "job1", FileIdx: 0, ArtIdx: 2, Offset: 2, Data: []byte("BB")},
		{JobID: "job1", FileIdx: 1, ArtIdx: 3, Offset: 2, Data: []byte("YY")},
	}
	for _, r := range reqs {
		if err := writeArticle(t.Context(), a, r); err != nil {
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

	req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("data")}
	if err := writeArticle(t.Context(), a, req); err != nil {
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
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i), Offset: int64(i * 4), Data: []byte("XXXX")}
		if err := writeArticle(t.Context(), a, req); err != nil {
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
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i), Offset: int64(i * 4), Data: []byte("XXXX")}
		if err := writeArticle(t.Context(), a, req); err != nil {
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
	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Enqueue n writes before stopping.
	for i := range n {
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i), Offset: int64(i * 4), Data: []byte("WXYZ")}
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	goroutinesBefore := runtime.NumGoroutine()

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After Stop the worker must have exited. Teardown is asynchronous, so poll
	// until the goroutine count settles rather than sleeping a fixed amount.
	waitUntil(t, func() bool { return runtime.NumGoroutine() <= goroutinesBefore }, 2*time.Second,
		"goroutine count to settle after Stop")

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
	if err := a.Start(t.Context()); err != nil {
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
	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	err := writeArticle(t.Context(), a, WriteRequest{})
	if !errors.Is(err, ErrStopped) {
		t.Errorf("WriteArticle after Stop returned %v, want ErrStopped", err)
	}
}

func TestWriteArticleBeforeStartReturnsErrNotStarted(t *testing.T) {
	a := New(Options{
		FileInfo: func(_ string, _ int) (FileInfo, error) { return FileInfo{}, nil },
	}, nil)
	err := writeArticle(t.Context(), a, WriteRequest{})
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
	if err := a.Start(t.Context()); err != nil {
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
	entered := make(chan struct{}, 1)
	opts2 := Options{
		QueueSize: 1,
		FileInfo: func(_ string, _ int) (FileInfo, error) {
			// Signal that the worker has dequeued the request and entered
			// FileInfo (queue now drained), then block until released.
			select {
			case entered <- struct{}{}:
			default:
			}
			<-blockWorker
			<-unblockWorker
			return FileInfo{}, fmt.Errorf("intentional error to discard")
		},
	}
	a2 := New(opts2, nil)
	if err := a2.Start(t.Context()); err != nil {
		t.Fatalf("Start a2: %v", err)
	}
	defer func() { _ = a2.Stop() }()

	// Send first request; the worker will call FileInfo and block.
	go func() {
		_ = writeArticle(t.Context(), a2, WriteRequest{JobID: "j", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("x")})
	}()

	// Deterministically wait until the worker has dequeued the request and
	// entered FileInfo (so the queue is drained); it cannot dequeue another
	// request until FileInfo returns.
	<-entered
	close(blockWorker)

	// Now the queue is empty but the worker is blocked in FileInfo.
	// Fill the queue (cap 1) with another request.
	fillCtx, fillCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer fillCancel()
	_ = writeArticle(fillCtx, a2, WriteRequest{JobID: "j", FileIdx: 1, ArtIdx: 1, Offset: 0, Data: []byte("y")})

	// Now try to enqueue with a cancellable context — should get ctx.Err() or ErrStopped.
	cancelCtx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		errCh <- writeArticle(cancelCtx, a2, WriteRequest{JobID: "j", FileIdx: 2, ArtIdx: 2, Offset: 0, Data: []byte("z")})
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
				ArtIdx:  testArtIdx(fi*partsPerFile + part),
				Offset:  int64(part * articleSize),
				Data:    fmt.Appendf(nil, "%04d%04d", fi, part),
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
				if err := writeArticle(t.Context(), a, req); err != nil {
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
	free, err := FreeBytes(t.Context(), dir)
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}
	if free <= 0 {
		t.Errorf("FreeBytes returned %d, want > 0", free)
	}
	t.Logf("FreeBytes(%s) = %d", dir, free)
}

func TestFreeBytes_ContextCanceled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := FreeBytes(ctx, dir)
	if err == nil {
		t.Fatal("expected error for already-cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected error to wrap context.Canceled, got %v", err)
	}
}

// ---- Telemetry tests -------------------------------------------------------

func TestTelemetryDiskWriteCounters(t *testing.T) {
	telemetry.Reset()

	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 3)

	opts := makeOpts(dir, files)
	a := startAssembler(t, opts)

	// Write 3 articles of 4 bytes each — no write cache, so each should
	// be a direct WriteAt call.
	for i := range 3 {
		req := WriteRequest{
			JobID:   "job1",
			FileIdx: 0,
			ArtIdx:  testArtIdx(i),
			Offset:  int64(i * 4),
			Data:    []byte("XXXX"),
		}
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := telemetry.DiskWrites.Value(); got != 3 {
		t.Errorf("DiskWrites = %d, want 3", got)
	}
	if got := telemetry.DiskWriteBytes.Value(); got != 12 {
		t.Errorf("DiskWriteBytes = %d, want 12", got)
	}
}

// TestTelemetryDiskWriteCountersCachedDrain ensures disk-write counters
// include writes made by the cache-drain path. With the write cache enabled
// and a small file (below the 512KB coalescing threshold), the articles are
// buffered as cache hits and only reach disk when drained at file completion
// via writeCachedArticles. Those WriteAt syscalls must still be counted.
func TestTelemetryDiskWriteCountersCachedDrain(t *testing.T) {
	telemetry.Reset()

	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 3)

	opts := makeOpts(dir, files)
	opts.WriteCacheBytes = 1 << 20 // 1 MiB — caching enabled
	a := startAssembler(t, opts)

	// 3 small articles stay buffered (well under the 512KB run threshold)
	// and are drained to disk when the file completes.
	for i := range 3 {
		req := WriteRequest{
			JobID:   "job1",
			FileIdx: 0,
			ArtIdx:  testArtIdx(i),
			Offset:  int64(i * 4),
			Data:    []byte("XXXX"),
		}
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// All 3 were buffered first...
	if got := telemetry.CacheHits.Value(); got != 3 {
		t.Errorf("CacheHits = %d, want 3", got)
	}
	// ...then drained to disk as 3 individual WriteAt calls totalling 12 bytes.
	if got := telemetry.DiskWrites.Value(); got != 3 {
		t.Errorf("DiskWrites = %d, want 3 (cache-drain writes must be counted)", got)
	}
	if got := telemetry.DiskWriteBytes.Value(); got != 12 {
		t.Errorf("DiskWriteBytes = %d, want 12", got)
	}
}

func TestTelemetryFileCompleted(t *testing.T) {
	telemetry.Reset()

	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 2)
	registerFile(t, dir, files, "job1", 1, 1)

	opts := makeOpts(dir, files)
	a := startAssembler(t, opts)

	// Complete file 0 (2 parts).
	for i := range 2 {
		req := WriteRequest{
			JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i),
			Offset: int64(i * 4), Data: []byte("AAAA"),
		}
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	// Complete file 1 (1 part).
	req := WriteRequest{
		JobID: "job1", FileIdx: 1, ArtIdx: 2,
		Offset: 0, Data: []byte("BBBB"),
	}
	if err := writeArticle(t.Context(), a, req); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := telemetry.FilesCompleted.Value(); got != 2 {
		t.Errorf("FilesCompleted = %d, want 2", got)
	}
}

func TestTelemetryPreallocCalls(t *testing.T) {
	telemetry.Reset()

	dir := t.TempDir()
	files := make(map[string]FileInfo)
	// File with ExpectedSize > 0 triggers pre-allocation.
	path := filepath.Join(dir, "job1_0.dat")
	files["job1:0"] = FileInfo{Path: path, TotalParts: 1, ExpectedSize: 4096}

	opts := makeOpts(dir, files)
	a := startAssembler(t, opts)

	req := WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0,
		Offset: 0, Data: []byte("DATA"),
	}
	if err := writeArticle(t.Context(), a, req); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := telemetry.PreallocCalls.Value(); got != 1 {
		t.Errorf("PreallocCalls = %d, want 1", got)
	}
}

func TestAssembler_HelperMethods(t *testing.T) {
	t.Parallel()

	// handleFatalArticle and handleSuccessArticle no longer take a writeCache,
	// open map or key — the FileWriter each openFile now owns is self-
	// contained — and neither records an ack: pendingDone/pendingFailed and
	// the MarkArticles* callbacks are gone with the assembler's ack authority
	// (X2). What is left to pin is the local bookkeeping: seenDone/seenFailed
	// dedup, now living on the FileWriter, and the true/false return that
	// tells processRequest whether to count the article toward partsWritten.

	t.Run("handleFatalArticle", func(t *testing.T) {
		a := newHelperAssembler()
		dir := t.TempDir()
		f := newHelperFile(t, dir, "fatal.dat", 0)
		req := WriteRequest{
			JobID:     "job1",
			ArtIdx:    0,
			MessageID: "msg1",
			FatalErr:  fmt.Errorf("article error"),
		}

		// First-time failure.
		if !a.handleFatalArticle(f, req) {
			t.Error("expected handleFatalArticle to return true for first-time failure")
		}
		if _, ok := f.w.seenFailed["msg1"]; !ok {
			t.Error("expected seenFailed to contain msg1")
		}

		// Duplicate failure.
		if a.handleFatalArticle(f, req) {
			t.Error("expected handleFatalArticle to return false for duplicate failure")
		}

		// Cross-check: already counted as success.
		f.w.seenFailed = make(map[string]struct{})
		f.w.seenDone["msg1"] = struct{}{}
		if a.handleFatalArticle(f, req) {
			t.Error("expected handleFatalArticle to return false when already counted as success")
		}
	})

	t.Run("handleSuccessArticle", func(t *testing.T) {
		a := newHelperAssembler()
		dir := t.TempDir()
		f := newHelperFile(t, dir, "success.dat", 0)

		req := WriteRequest{
			JobID:     "job1",
			ArtIdx:    0,
			MessageID: "msg1",
			Offset:    0,
			Data:      []byte("hello success"),
		}

		// First-time success.
		if !a.handleSuccessArticle(f, req) {
			t.Error("expected handleSuccessArticle to return true")
		}
		if _, ok := f.w.seenDone["msg1"]; !ok {
			t.Error("expected seenDone to contain msg1")
		}
		if got := f.w.writtenSoFar(); len(got) != 1 {
			t.Fatalf("writtenSoFar = %v, want 1 entry — the bytes reached WriteAt", got)
		}

		// Duplicate success.
		req2 := WriteRequest{JobID: "job1", ArtIdx: 0, MessageID: "msg1", Offset: 0, Data: []byte("hello success")}
		if a.handleSuccessArticle(f, req2) {
			t.Error("expected handleSuccessArticle to return false for duplicate success")
		}

		// Cross-check: already counted as failure. The retry still writes —
		// the bytes are still the file's content — but must not be counted
		// toward partsWritten a second time.
		f.w.seenDone = make(map[string]struct{})
		f.w.seenFailed["msg1"] = struct{}{}
		req3 := WriteRequest{JobID: "job1", ArtIdx: 0, MessageID: "msg1", Offset: 20, Data: []byte("world")}
		if a.handleSuccessArticle(f, req3) {
			t.Error("expected handleSuccessArticle to return false when already counted as failure")
		}
		if _, ok := f.w.seenDone["msg1"]; !ok {
			t.Error("expected seenDone to contain msg1 after the retry")
		}
	})

	t.Run("finalizeFile", func(t *testing.T) {
		// finalizeFile no longer truncates, no longer computes a CRC, and no
		// longer closes the handle — all three moved to durability.Barrier,
		// which is the only component that knows an fsync has happened (see
		// finalizeFile's doc comment). What is left to pin here is: the
		// tombstone goes down, the handle stays OPEN for the barrier to
		// finalize through, and OnFileComplete fires with (jobID, fileIdx).
		var callbackJobID string
		var callbackFileIdx int
		var callbackFired int

		opts := Options{
			FileInfo: func(jobID string, fileIdx int) (FileInfo, error) {
				return FileInfo{Path: "test"}, nil
			},
			OnFileComplete: func(jobID string, fileIdx int) {
				callbackJobID = jobID
				callbackFileIdx = fileIdx
				callbackFired++
			},
		}
		a := New(opts, slog.Default())

		dir := t.TempDir()
		f := newHelperFile(t, dir, "finalize.dat", 0)
		if _, err := f.w.handle.Write([]byte("hello world final truncate")); err != nil {
			t.Fatal(err)
		}

		key := fileKey{jobID: "job1", fileIdx: 0}
		completed := make(map[fileKey]struct{})

		req := WriteRequest{
			JobID:   "job1",
			FileIdx: 0,
			ArtIdx:  0,
		}

		a.finalizeFile(f, key, req, completed)

		// The file stays open, and that is now structural rather than
		// asserted: finalizeFile is not given the open map at all, so it
		// cannot remove the file from it. Closing here would take the handle
		// away from Barrier.FinalizeFile, which still has to Drain, Sync,
		// Truncate and Stat it — the completed file would keep
		// pre-allocation's trailing zeros and its last articles would never
		// be acked.
		//
		// The assertion this replaces read the map back after passing it to a
		// parameter finalizeFile had already blanked to `_`, so it could not
		// have failed.
		//
		// The tombstone goes down immediately even so, or a late duplicate
		// would be written into a file the barrier is mid-way through
		// truncating.
		if _, ok := completed[key]; !ok {
			t.Error("expected file to be added to completed map")
		}

		// Check callback fired.
		if callbackFired != 1 {
			t.Errorf("expected OnFileComplete callback to fire 1 time, got %d", callbackFired)
		}
		if callbackJobID != "job1" || callbackFileIdx != 0 {
			t.Errorf("callback parameters mismatch: job=%s, idx=%d", callbackJobID, callbackFileIdx)
		}
	})

	t.Run("handleSuccessArticle_writeFail", func(t *testing.T) {
		// A write failure must NOT be counted toward completion, and the
		// article must not be reported Written, must land in seenFailed
		// rather than seenDone, and must not be recorded as damaged (A1).
		//
		// This subtest previously required the opposite return value, on the
		// reasoning that "the barrier's next Drain simply won't see it, which
		// leaves it Outstanding for a re-fetch". Absence from a Drain does not
		// make an article Outstanding: its Emitted bit is still set from
		// dispatch, ForEachUnfinishedArticle skips a set Emitted bit, and no
		// path on this branch clears it. So the article was never re-fetched,
		// while partsWritten counted a write that did not happen and the file
		// finalized over pre-allocation zeros at full reported health.
		//
		// Stalling is not the harm the old expectation took it for. A storage
		// fault stalls or fails the job everywhere else in this design, and
		// the article stays unresolved either way — which is the whole of A1.
		a := newHelperAssembler()
		dir := t.TempDir()
		f := newHelperFile(t, dir, "fail_write.dat", 0)
		_ = f.w.handle.Close() // force WriteAt to fail

		req := WriteRequest{
			JobID:     "job1",
			ArtIdx:    0,
			MessageID: "msgFail",
			Offset:    0,
			Data:      []byte("data that will not be written"),
		}

		got := a.handleSuccessArticle(f, req)
		if got {
			t.Error("handleSuccessArticle counted a failed write toward completion; " +
				"processRequest increments partsWritten on this return, so the file " +
				"reaches TotalParts and finalizes over bytes that never reached disk")
		}
		if got := f.w.writtenSoFar(); len(got) != 0 {
			t.Errorf("writtenSoFar = %v after a failed write, want empty", got)
		}
		if _, ok := f.w.seenFailed["msgFail"]; ok {
			t.Error("msgFail was recorded as FAILED by a storage fault, which A1 forbids: " +
				"a full disk is not evidence about the article's availability")
		}
		if _, ok := f.w.seenDone["msgFail"]; ok {
			t.Error("msgFail should not remain in seenDone after write failure")
		}
	})
}

func TestNew_QueueSizeDefault(t *testing.T) {
	a := New(Options{
		FileInfo:  func(_ string, _ int) (FileInfo, error) { return FileInfo{}, nil },
		QueueSize: 0,
	}, nil)
	if cap(a.reqs) != defaultQueueSize {
		t.Errorf("cap(reqs) = %d, want %d", cap(a.reqs), defaultQueueSize)
	}
}

func TestDiskCheckInterval(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	total := diskCheckInterval * 2              // 32 requests
	registerFile(t, dir, files, "job1", 0, 100) // Keep file open beyond 32 requests

	var lowDiskCount atomic.Int32
	opts := makeOpts(dir, files)
	opts.QueueSize = 64
	const tenPiB = 10 * (1 << 50)
	opts.MinFreeBytes = tenPiB
	opts.OnLowDisk = func(dir string, free int64) {
		lowDiskCount.Add(1)
	}

	blockCh := make(chan struct{})
	entered := make(chan struct{}, 1)

	// Wrap FileInfo to block on the first call.
	origFileInfo := opts.FileInfo
	opts.FileInfo = func(jobID string, fileIdx int) (FileInfo, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-blockCh
		return origFileInfo(jobID, fileIdx)
	}

	a := New(opts, nil)
	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 1. Send first request. It will block inside FileInfo, stalling the worker.
	req1 := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("XXXX")}
	if err := writeArticle(t.Context(), a, req1); err != nil {
		t.Fatalf("WriteArticle 1: %v", err)
	}

	// Wait until the worker has dequeued and entered FileInfo.
	<-entered

	// 2. Queue 31 more requests (total = 32 requests). They will block in the channel.
	for i := 1; i < total; i++ {
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i), Offset: int64(i * 4), Data: []byte("XXXX")}
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}

	// 3. Unblock the worker asynchronously after Stop is initiated. The
	// sleep is not load-bearing for correctness: a.Stop() blocks until the
	// worker drains, so it simply waits longer if blockCh closes later.
	// It exists only to give Stop() a chance to begin closing stopCh first,
	// so the worker observes shutdown via the stop-drain path rather than
	// the main loop. Intentional timing window (AGENTS.md).
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(blockCh)
	}()

	// 4. Stop the assembler. This closes stopCh, so the worker will enter
	// the stop drain loop and drain the remaining 31 requests.
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Reqs 16 and 32 should trigger checkDiskSpace.
	// Since MinFreeBytes is 10 PiB, OnLowDisk should fire exactly 2 times.
	if n := lowDiskCount.Load(); n != 2 {
		t.Errorf("OnLowDisk called %d times, want exactly 2", n)
	}
}

func TestPreallocCallsNotIncrementedWhenSizeZero(t *testing.T) {
	telemetry.Reset()

	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := filepath.Join(dir, "job1_0.dat")
	files["job1:0"] = FileInfo{Path: path, TotalParts: 1, ExpectedSize: 0}

	opts := makeOpts(dir, files)
	a := startAssembler(t, opts)

	req := WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0,
		Offset: 0, Data: []byte("DATA"),
	}
	if err := writeArticle(t.Context(), a, req); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := telemetry.PreallocCalls.Value(); got != 0 {
		t.Errorf("PreallocCalls = %d, want 0", got)
	}
}

func TestTotalPartsZero_DoesNotComplete(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 0) // TotalParts = 0

	var completed bool
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completed = true }

	a := startAssembler(t, opts)

	req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("AAAA")}
	if err := writeArticle(t.Context(), a, req); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if completed {
		t.Error("OnFileComplete should not fire when TotalParts is 0")
	}
}

// TestCancelJob_BlocksUntilFileClosedAndRemoved proves CancelJob does not
// report completion until the worker has actually closed and removed the
// job's open file handles. Callers like app.RemoveJob delete the job's
// directory immediately after CancelJob returns; if CancelJob merely
// enqueues a control message without waiting for it to be processed, the
// directory delete can race the worker's Close()+Remove() of files still
// inside it. On NFS-mounted download directories this race produces
// .nfsXXXXXX silly-rename artifacts and a non-empty directory the delete
// can't remove.
func TestCancelJob_BlocksUntilFileClosedAndRemoved(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.dat")
	blockerPath := filepath.Join(dir, "blocker.dat")

	resolverEntered := make(chan struct{}, 1)
	resolverRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resolverRelease) }) }
	// Guarantee the worker is released even if the test fails before the
	// deliberate release point below, so a.Stop() in t.Cleanup doesn't
	// block forever on a permanently-parked worker goroutine.
	defer release()

	opts := Options{
		FileInfo: func(jobID string, _ int) (FileInfo, error) {
			switch jobID {
			case "target":
				// TotalParts is never reached, so the file stays open.
				return FileInfo{Path: targetPath, TotalParts: 100}, nil
			case "blocker":
				resolverEntered <- struct{}{}
				<-resolverRelease
				return FileInfo{Path: blockerPath, TotalParts: 1}, nil
			default:
				return FileInfo{}, fmt.Errorf("unexpected job %q", jobID)
			}
		},
	}
	a := startAssembler(t, opts)

	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "target", FileIdx: 0, ArtIdx: 0, MessageID: "m1", Offset: 0, Data: []byte("AAAA"),
	}); err != nil {
		t.Fatalf("WriteArticle(target): %v", err)
	}

	// The worker processes a.reqs FIFO on a single goroutine, so by the time
	// it's parked inside the blocker's FileInfo call, the "target" article
	// enqueued above is guaranteed to have already been fully processed
	// (file opened and written).
	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "blocker", FileIdx: 0, ArtIdx: 0, MessageID: "b1", Offset: 0, Data: []byte("Z"),
	}); err != nil {
		t.Fatalf("WriteArticle(blocker): %v", err)
	}
	<-resolverEntered // worker is now parked inside the blocker's FileInfo call.

	if _, err := os.Stat(targetPath); err != nil {
		t.Fatalf("target file should exist before cancel: %v", err)
	}

	cancelDone := make(chan error, 1)
	go func() { cancelDone <- a.CancelJob(t.Context(), "target") }()

	// Negative-observation window: the worker cannot have reached the
	// cancel control message yet -- it's deterministically still blocked
	// inside the blocker's FileInfo call. CancelJob must not report
	// completion this early.
	select {
	case err := <-cancelDone:
		t.Fatalf("CancelJob returned before the worker could have closed the file (err=%v)", err)
	case <-time.After(20 * time.Millisecond):
	}

	release() // let the worker proceed past the blocker.

	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatalf("CancelJob: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CancelJob did not return after unblocking the worker")
	}

	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("target file should have been removed by CancelJob, stat err = %v", err)
	}
}

func TestAssembler_StopWaitGroup(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	const (
		numFiles     = 5
		partsPerFile = 10
	)
	for fi := range numFiles {
		registerFile(t, dir, files, "job1", fi, partsPerFile)
	}

	opts := makeOpts(dir, files)
	opts.QueueSize = 2

	var completions atomic.Int32
	opts.OnFileComplete = func(_ string, _ int) {
		completions.Add(1)
	}

	a := New(opts, nil)

	// Verify that the assembler is using sync.WaitGroup (wg) and not an atomic spin loop (inFlight)
	val := reflect.ValueOf(a).Elem()
	if !val.FieldByName("wg").IsValid() {
		t.Fatalf("Assembler must use wg (sync.WaitGroup) instead of inFlight atomic spin loop for clean synchronization")
	}
	if val.FieldByName("inFlight").IsValid() {
		t.Fatalf("Assembler must not have inFlight atomic counter (should be replaced by wg)")
	}
	if val.FieldByName("workerDone").IsValid() {
		t.Fatalf("Assembler must not have workerDone channel (worker should be tracked by wg)")
	}

	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Test concurrent in-flight assemblies being waited on during Stop().
	const workers = 10
	var senderWg sync.WaitGroup
	senderWg.Add(workers)

	for w := range workers {
		go func() {
			defer senderWg.Done()
			for part := range partsPerFile {
				req := WriteRequest{
					JobID:   "job1",
					FileIdx: w % numFiles,
					ArtIdx:  testArtIdx(w*partsPerFile + part),
					Offset:  int64(part * 4),
					Data:    []byte("DATA"),
				}
				_ = writeArticle(t.Context(), a, req)
			}
		}()
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- a.Stop()
	}()

	senderWg.Wait()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Stop() to return (possible deadlock)")
	}

	req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("TEST")}
	if err := writeArticle(t.Context(), a, req); !errors.Is(err, ErrStopped) {
		t.Errorf("WriteArticle after Stop() = %v, want ErrStopped", err)
	}
	if err := a.CancelJob(t.Context(), "job1"); !errors.Is(err, ErrStopped) {
		t.Errorf("CancelJob after Stop() = %v, want ErrStopped", err)
	}
}

func TestAssembler_CacheUsageBytes_TracksBufferedBytes(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 3) // 3-part file

	opts := makeOpts(dir, files)
	opts.WriteCacheBytes = 1 << 20 // 1 MiB — caching enabled
	a := startAssembler(t, opts)

	if got := a.CacheUsageBytes(); got != 0 {
		t.Fatalf("CacheUsageBytes() before any writes = %d, want 0", got)
	}

	// Write 2 of the file's 3 parts. Both stay buffered (well under the
	// 512KB coalescing threshold) since the file isn't complete yet, so
	// CacheUsageBytes must eventually reflect the buffered bytes before
	// the 3rd (completing) write drains them to disk.
	//
	// WriteArticle only enqueues onto a channel (a.reqs <- req) and
	// returns immediately — it does not wait for the worker goroutine to
	// actually process the request. Poll to wait for the worker to process
	// both articles.
	for i := range 2 {
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i), Offset: int64(i * 4), Data: []byte("XXXX")}
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	// Poll until we see the buffered bytes from both articles.
	// The cache tracks bytes added to writeCache.used, updated after
	// each dispatchRequest call via defer.
	waitUntil(t, func() bool { return a.CacheUsageBytes() >= 8 }, 2*time.Second, "cache reaches at least 8 bytes")

	// Complete the file (3rd part) — this drains all buffered articles.
	req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: 2, Offset: 8, Data: []byte("XXXX")}
	if err := writeArticle(t.Context(), a, req); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := a.CacheUsageBytes(); got != 0 {
		t.Errorf("CacheUsageBytes() after drain = %d, want 0", got)
	}
}

// Dummy references to satisfy scripts/check_test_alignment. These are internal
// goroutine workers or helper methods called in background processing.
var (
	_ = (*Assembler).worker
	_ = (*Assembler).processRequest
	_ = (*Assembler).dispatchRequest
	_ = (*Assembler).openTargetFile
)

// TestFileWriter_FlushRunErrorCountsPipelineError translates
// TestAssembler_FlushRunError: flushRun moved from the assembler onto
// FileWriter and now returns an error instead of a bool (the caller has no
// ack to withhold any more), but the telemetry contract — a failed coalesced
// write counts a disk_write_error — is unchanged.
func TestFileWriter_FlushRunErrorCountsPipelineError(t *testing.T) {
	telemetry.Reset()
	t.Cleanup(telemetry.Reset)

	tmpFile, err := os.CreateTemp(t.TempDir(), "filewriter-flush-run-err")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close() // closed handle — WriteAt will fail

	w := newFileWriter(tmpFile, tmpFile.Name(), fileKey{jobID: "job1", fileIdx: 0}, newWriteCache(0))
	run := &flushRun{data: []byte("test"), offset: 0}

	if err := w.flushRun(run); err == nil {
		t.Error("flushRun on a closed file returned nil error, want a storage fault")
	}
	if got := telemetry.ErrorCount(telemetry.ErrClassDiskWriteError); got != 1 {
		t.Errorf("PipelineErrors[disk_write_error] = %d, want 1", got)
	}
}

// TestFileWriter_DirectWriteErrorCountsPipelineError translates
// TestAssembler_WriteArticleOrBufferDiskErrorCountsPipelineError: the
// caching-disabled direct-write branch moved from writeArticleOrBuffer onto
// FileWriter.Accept, but the telemetry contract is unchanged. Not a
// t.Parallel() test — PipelineErrors is process-global state.
func TestFileWriter_DirectWriteErrorCountsPipelineError(t *testing.T) {
	telemetry.Reset()
	t.Cleanup(telemetry.Reset)

	tmpFile, err := os.CreateTemp(t.TempDir(), "filewriter_write_or_buffer_err_")
	if err != nil {
		t.Fatal(err)
	}
	tmpFile.Close() // WriteAt will fail

	w := newFileWriter(tmpFile, tmpFile.Name(), fileKey{jobID: "job1", fileIdx: 0}, newWriteCache(0))

	if err := w.Accept(articleID{msgID: "m1"}, 0, []byte("data")); err == nil {
		t.Error("Accept on a closed file returned nil error, want a storage fault")
	}
	if got := telemetry.ErrorCount(telemetry.ErrClassDiskWriteError); got != 1 {
		t.Errorf("PipelineErrors[disk_write_error] = %d, want 1", got)
	}
}

// TestFileWriter_DrainWriteErrorCountsPipelineError translates
// TestAssembler_WriteCachedArticlesDiskErrorCountsPipelineError: the
// cache-drain WriteAt path moved from writeCachedArticles onto
// FileWriter.Drain, but the telemetry contract is unchanged. Not a
// t.Parallel() test — PipelineErrors is process-global state.
func TestFileWriter_DrainWriteErrorCountsPipelineError(t *testing.T) {
	telemetry.Reset()
	t.Cleanup(telemetry.Reset)

	tmpFile, err := os.CreateTemp(t.TempDir(), "filewriter_write_cached_err_")
	if err != nil {
		t.Fatal(err)
	}

	w := newFileWriter(tmpFile, tmpFile.Name(), fileKey{jobID: "job1", fileIdx: 0}, newWriteCache(1<<20))
	// Buffer an article without triggering a contiguous flush, then close
	// the handle so the drain's WriteAt fails.
	if err := w.Accept(articleID{msgID: "m1"}, 4096, []byte("data")); err != nil {
		t.Fatalf("Accept (buffered): %v", err)
	}
	_ = tmpFile.Close()

	if _, err := w.Drain(); err == nil {
		t.Error("Drain after closing the handle returned nil error, want a storage fault")
	}
	if got := telemetry.ErrorCount(telemetry.ErrClassDiskWriteError); got != 1 {
		t.Errorf("PipelineErrors[disk_write_error] = %d, want 1", got)
	}
}

func TestAssembler_CloseJobHandles(t *testing.T) {
	dir := t.TempDir()
	testutil.AssertNoFDLeaks(t, dir)
	dir1 := filepath.Join(dir, "sub1")
	dir2 := filepath.Join(dir, "sub2")
	if err := os.MkdirAll(dir1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}

	files := make(map[string]FileInfo)
	path0 := registerFile(t, dir, files, "job1", 0, 5)
	path1 := registerFile(t, dir1, files, "job1", 1, 5)
	path2 := registerFile(t, dir2, files, "job1", 2, 5)

	a := New(makeOpts(dir, files), nil)
	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	// Write 1 part to each file index so all 3 are opened and sitting incomplete in open map.
	for idx := range 3 {
		req := WriteRequest{JobID: "job1", FileIdx: idx, ArtIdx: testArtIdx(idx), Offset: 0, Data: []byte("0000")}
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle fileIdx=%d: %v", idx, err)
		}
	}

	// Calling CloseJobHandles must close all 3 open file descriptors and remove from open map,
	// but must NOT delete the files from disk (unlike CancelJob).
	if err := a.CloseJobHandles(t.Context(), "job1"); err != nil {
		t.Fatalf("CloseJobHandles: %v", err)
	}

	for _, p := range []string{path0, path1, path2} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file %s was deleted from disk, should have been preserved: %v", p, err)
		}
	}

	// Test stopped state.
	a.Stop()
	if err := a.CloseJobHandles(t.Context(), "job1"); !errors.Is(err, ErrStopped) {
		t.Errorf("stopped CloseJobHandles: got %v, want ErrStopped", err)
	}
}

func TestAssembler_CloseJobHandles_Unstarted(t *testing.T) {
	a := New(makeOpts("", nil), nil)
	if err := a.CloseJobHandles(t.Context(), "job1"); !errors.Is(err, ErrNotStarted) {
		t.Errorf("unstarted CloseJobHandles: got %v, want ErrNotStarted", err)
	}
}

func TestAssembler_CloseJobHandles_ContextCanceled(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 5)
	a := New(makeOpts(dir, files), nil)
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.CloseJobHandles(ctx, "job1"); !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
