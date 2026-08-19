package assembler

import (
	"fmt"
	"testing"
	"unsafe"
)

func TestWriteCacheDisabledWhenZero(t *testing.T) {
	wc := newWriteCache(0)
	if wc.enabled() {
		t.Error("expected disabled with limit=0")
	}
	key := fileKey{jobID: "j", fileIdx: 0}
	if bufferAt(wc, key, 0, []byte("data")) {
		t.Error("buffer should return false when disabled")
	}
}

func TestWriteCacheBuffer(t *testing.T) {
	wc := newWriteCache(1 << 20) // 1 MiB
	key := fileKey{jobID: "j", fileIdx: 0}

	if !bufferAt(wc, key, 0, make([]byte, 1000)) {
		t.Fatal("buffer should return true")
	}
	if wc.used != 1000 {
		t.Errorf("used = %d, want 1000", wc.used)
	}
	if wc.bytesFor(key) != 1000 {
		t.Errorf("bytesFor = %d, want 1000", wc.bytesFor(key))
	}

	// Buffer a second article at a different offset.
	if !bufferAt(wc, key, 1000, make([]byte, 500)) {
		t.Fatal("buffer should return true")
	}
	if wc.used != 1500 {
		t.Errorf("used = %d, want 1500", wc.used)
	}
}

func TestWriteCacheContiguousFlush(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}

	// Buffer articles that form a contiguous run from offset 0.
	// Each 200KB, so 3 articles = 600KB > 512KB threshold.
	artSize := 200 * 1024
	for i := range 3 {
		data := make([]byte, artSize)
		// Write a marker byte so we can verify coalescing.
		data[0] = byte(i + 1)
		bufferAt(wc, key, int64(i*artSize), data)
	}

	run := wc.flushContiguous(key)
	if run == nil {
		t.Fatal("expected a contiguous run")
	}
	if run.offset != 0 {
		t.Errorf("run offset = %d, want 0", run.offset)
	}
	expectedLen := 3 * artSize
	if len(run.data) != expectedLen {
		t.Errorf("run length = %d, want %d", len(run.data), expectedLen)
	}
	// Verify marker bytes.
	for i := range 3 {
		if run.data[i*artSize] != byte(i+1) {
			t.Errorf("marker at %d = %d, want %d", i*artSize, run.data[i*artSize], i+1)
		}
	}
	// After flush, used should be 0.
	if wc.used != 0 {
		t.Errorf("used after flush = %d, want 0", wc.used)
	}
}

func TestWriteCacheContiguousFlushBelowThreshold(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}

	// Buffer a single small article — below the 512KB threshold.
	bufferAt(wc, key, 0, make([]byte, 1024))

	run := wc.flushContiguous(key)
	if run != nil {
		t.Error("expected no flush for small contiguous run")
	}
	if wc.used != 1024 {
		t.Errorf("used = %d, want 1024 (should still be buffered)", wc.used)
	}
}

func TestWriteCacheContiguousFlushWithGap(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}

	artSize := 300 * 1024
	// Article at offset 0 and offset 600KB (gap at 300KB).
	bufferAt(wc, key, 0, make([]byte, artSize))
	bufferAt(wc, key, int64(2*artSize), make([]byte, artSize))

	// Only the run from offset 0 is contiguous, but it's only 300KB < 512KB.
	run := wc.flushContiguous(key)
	if run != nil {
		t.Error("expected no flush — contiguous run from cursor is below threshold")
	}
}

func TestWriteCachePressure(t *testing.T) {
	wc := newWriteCache(1000)

	if wc.pressure() {
		t.Error("should not be under pressure when empty")
	}

	// 800/1000 = 80% (should be false, also tests /9 arithmetic mutation)
	wc.used = 800
	if wc.pressure() {
		t.Error("should not be under pressure at 80%")
	}

	// 900/1000 = 90% (boundary: > 90% is required, so exactly 90% should be false)
	wc.used = 900
	if wc.pressure() {
		t.Error("should not be under pressure at exactly 90%")
	}

	// 901/1000 = 90.1% (should be true)
	wc.used = 901
	if !wc.pressure() {
		t.Error("should be under pressure at 90.1%")
	}
}

func TestWriteCacheForceFlushLargest(t *testing.T) {
	wc := newWriteCache(1 << 20)
	k1 := fileKey{jobID: "j", fileIdx: 0}
	k2 := fileKey{jobID: "j", fileIdx: 1}

	bufferAt(wc, k1, 0, make([]byte, 1000))
	bufferAt(wc, k2, 0, make([]byte, 2000))

	fk, arts := wc.forceFlushLargest()
	if fk != k2 {
		t.Errorf("expected largest = %v, got %v", k2, fk)
	}
	if len(arts) != 1 {
		t.Errorf("expected 1 article, got %d", len(arts))
	}
	if len(arts[0].data) != 2000 {
		t.Errorf("article size = %d, want 2000", len(arts[0].data))
	}
	// Only k1 should remain.
	if wc.used != 1000 {
		t.Errorf("used after force-flush = %d, want 1000", wc.used)
	}
}

func TestWriteCacheDrainFile(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}

	// Buffer articles out of order.
	bufferAt(wc, key, 2000, make([]byte, 500))
	bufferAt(wc, key, 0, make([]byte, 1000))
	bufferAt(wc, key, 1000, make([]byte, 1000))

	_, arts := wc.drainFile(key)
	if len(arts) != 3 {
		t.Fatalf("expected 3 articles, got %d", len(arts))
	}
	// Should be sorted by offset.
	if arts[0].offset != 0 {
		t.Errorf("arts[0].offset = %d, want 0", arts[0].offset)
	}
	if arts[1].offset != 1000 {
		t.Errorf("arts[1].offset = %d, want 1000", arts[1].offset)
	}
	if arts[2].offset != 2000 {
		t.Errorf("arts[2].offset = %d, want 2000", arts[2].offset)
	}
	if wc.used != 0 {
		t.Errorf("used after drain = %d, want 0", wc.used)
	}
}

func TestWriteCacheDrainAll(t *testing.T) {
	wc := newWriteCache(1 << 20)
	k1 := fileKey{jobID: "j", fileIdx: 0}
	k2 := fileKey{jobID: "j", fileIdx: 1}

	bufferAt(wc, k1, 0, make([]byte, 100))
	bufferAt(wc, k2, 0, make([]byte, 200))

	// Add an empty file entry to perFile and ensure it is not returned.
	wc.perFile[fileKey{jobID: "empty", fileIdx: 0}] = &fileBuf{
		articles: make(map[int64]bufferedArticle),
	}

	result := wc.drainAll()
	if len(result) != 2 {
		t.Errorf("expected 2 files, got %d", len(result))
	}
	if _, ok := result[fileKey{jobID: "empty", fileIdx: 0}]; ok {
		t.Error("drainAll should not return entries for empty file buffers")
	}
	if wc.used != 0 {
		t.Errorf("used after drainAll = %d, want 0", wc.used)
	}
	if len(wc.perFile) != 0 {
		t.Errorf("perFile should be empty after drainAll")
	}
}

func TestWriteCacheForget(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}

	bufferAt(wc, key, 0, make([]byte, 500))
	wc.forget(key)

	if wc.used != 0 {
		t.Errorf("used after forget = %d, want 0", wc.used)
	}
	if _, ok := wc.perFile[key]; ok {
		t.Error("perFile should not contain forgotten key")
	}
}

func TestWriteCacheDuplicateOffset(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}

	bufferAt(wc, key, 0, make([]byte, 1000))
	bufferAt(wc, key, 0, make([]byte, 500)) // replace

	if wc.used != 500 {
		t.Errorf("used = %d, want 500 (should replace)", wc.used)
	}
	if wc.bytesFor(key) != 500 {
		t.Errorf("bytesFor = %d, want 500", wc.bytesFor(key))
	}
}

func TestWriteCacheWriteCursorAdvances(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}

	artSize := 200 * 1024
	// Buffer 4 contiguous articles = 800KB > 512KB threshold.
	for i := range 4 {
		bufferAt(wc, key, int64(i*artSize), make([]byte, artSize))
	}

	run := wc.flushContiguous(key)
	if run == nil {
		t.Fatal("expected flush")
	}

	// Write cursor should have advanced.
	fb := wc.perFile[key]
	expectedCursor := int64(4 * artSize)
	if fb != nil && fb.writeCursor != expectedCursor {
		t.Errorf("writeCursor = %d, want %d", fb.writeCursor, expectedCursor)
	}

	// Buffer more starting from where we left off.
	for i := 4; i < 8; i++ {
		bufferAt(wc, key, int64(i*artSize), make([]byte, artSize))
	}

	run = wc.flushContiguous(key)
	if run == nil {
		t.Fatal("expected second flush")
	}
	if run.offset != expectedCursor {
		t.Errorf("second run offset = %d, want %d", run.offset, expectedCursor)
	}
}

func TestWriteCachePressureDisabledWhenZeroLimit(t *testing.T) {
	wc := newWriteCache(0)
	wc.used = 100
	if wc.pressure() {
		t.Error("pressure should be false when limit=0")
	}
}

// ---------- Assembler integration tests with write coalescing ----------

func TestAssemblerWithWriteCache_BasicCoalescing(t *testing.T) {
	// With caching enabled, articles should be buffered and the final
	// file contents should be identical to the non-cached path.
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 3)

	var completed bool
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completed = true }
	opts.WriteCacheBytes = 1 << 20 // 1 MiB cache

	a := startAssembler(t, opts)

	// Write 3 contiguous articles.
	for i := range 3 {
		data := fmt.Appendf(nil, "PT%02d", i)
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: int32(i), Offset: int64(i * 4), Data: data} //nolint:gosec // G115: loop bound is small
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !completed {
		t.Error("OnFileComplete was not called")
	}

	data := readFile(t, path)
	want := "PT00PT01PT02"
	if string(data) != want {
		t.Errorf("file content = %q, want %q", data, want)
	}
}

func TestAssemblerWithWriteCache_OutOfOrder(t *testing.T) {
	// Articles arriving out of order should still produce correct output
	// when caching is enabled. The cache drains on file completion.
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 4)

	var completed bool
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completed = true }
	opts.WriteCacheBytes = 1 << 20 // 1 MiB

	a := startAssembler(t, opts)

	// Deliberately out-of-order offsets.
	articles := []struct {
		offset int64
		data   string
	}{
		{12, "DDDD"},
		{0, "AAAA"},
		{8, "CCCC"},
		{4, "BBBB"},
	}
	for i, art := range articles {
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: int32(i), Offset: art.offset, Data: []byte(art.data)} //nolint:gosec // G115: loop bound is small
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !completed {
		t.Error("OnFileComplete was not called")
	}

	data := readFile(t, path)
	want := "AAAABBBBCCCCDDDD"
	if string(data) != want {
		t.Errorf("file content = %q, want %q", data, want)
	}
}

func TestAssemblerWithWriteCache_PressureFlush(t *testing.T) {
	// Set a tiny cache limit to trigger pressure flush. Articles should
	// still be written correctly.
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 10)

	var completed bool
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completed = true }
	opts.WriteCacheBytes = 100 // only 100 bytes — triggers pressure quickly

	a := startAssembler(t, opts)

	// Write 10 articles of 20 bytes each = 200 bytes total.
	// This exceeds the 100-byte cache limit, forcing pressure flushes.
	for i := range 10 {
		data := make([]byte, 20)
		data[0] = byte(i + 1)                                                                               // marker
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: int32(i), Offset: int64(i * 20), Data: data} //nolint:gosec // G115: loop bound is 10
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !completed {
		t.Error("OnFileComplete was not called")
	}

	data := readFile(t, path)
	if len(data) != 200 {
		t.Fatalf("file length = %d, want 200", len(data))
	}
	// Verify marker bytes.
	for i := range 10 {
		if data[i*20] != byte(i+1) {
			t.Errorf("marker at %d = %d, want %d", i*20, data[i*20], i+1)
		}
	}
}

func TestAssemblerWithWriteCache_MultipleFiles(t *testing.T) {
	// Multiple files interleaved with caching enabled.
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	pathA := registerFile(t, dir, files, "job1", 0, 2)
	pathB := registerFile(t, dir, files, "job1", 1, 2)

	completionCount := 0
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completionCount++ }
	opts.WriteCacheBytes = 1 << 20

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

	if completionCount != 2 {
		t.Errorf("completions = %d, want 2", completionCount)
	}

	if string(readFile(t, pathA)) != "AABB" {
		t.Errorf("file A content = %q, want AABB", readFile(t, pathA))
	}
	if string(readFile(t, pathB)) != "XXYY" {
		t.Errorf("file B content = %q, want XXYY", readFile(t, pathB))
	}
}

func TestAssemblerWithWriteCache_ShutdownDrain(t *testing.T) {
	// When the assembler stops before file completion, cached articles
	// should be drained to disk so data is not lost.
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 100) // won't complete

	opts := makeOpts(dir, files)
	opts.WriteCacheBytes = 1 << 20

	a := startAssembler(t, opts)

	// Write 3 articles (of 100 needed), then stop.
	for i := range 3 {
		data := fmt.Appendf(nil, "D%03d", i)
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: int32(i), Offset: int64(i * 4), Data: data} //nolint:gosec // G115: loop bound is small
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// File should exist with the data written.
	data := readFile(t, path)
	want := "D000D001D002"
	if len(data) < len(want) {
		t.Fatalf("file too short: got %d bytes, want at least %d", len(data), len(want))
	}
	if string(data[:len(want)]) != want {
		t.Errorf("file content prefix = %q, want %q", data[:len(want)], want)
	}
}

// ---------- Direct Writecache Helpers ----------

func TestBuildContiguousRun_Direct(t *testing.T) {
	t.Parallel()
	wc := newWriteCache(1000)
	fb := &fileBuf{
		articles: make(map[int64]bufferedArticle),
	}

	// Case 1: Empty
	run := wc.buildContiguousRun(fb, 100)
	if run != nil {
		t.Error("expected nil run for empty buffer")
	}

	// Case 2: Contiguous but below minSize
	fb.articles[0] = bufferedArticle{offset: 0, data: []byte("short")}
	run = wc.buildContiguousRun(fb, 100)
	if run != nil {
		t.Error("expected nil run for run size < minSize")
	}

	// Case 2.1: Contiguous and exactly equal to minSize (boundary)
	fb.articles[0] = bufferedArticle{offset: 0, data: []byte("exactlyfive")}
	run = wc.buildContiguousRun(fb, 11) // runSize = 11, minSize = 11
	if run == nil {
		t.Error("expected contiguous run when runSize == minSize")
	}
	fb.writeCursor = 0

	// Case 3: Contiguous and >= minSize
	fb.articles[5] = bufferedArticle{offset: 5, data: []byte("enough data to exceed threshold")}
	fb.articles[0] = bufferedArticle{offset: 0, data: []byte("hello")}
	run = wc.buildContiguousRun(fb, 20)
	if run == nil {
		t.Fatal("expected contiguous run")
	}
	if run.offset != 0 {
		t.Errorf("offset = %d, want 0", run.offset)
	}
	expectedData := "helloenough data to exceed threshold"
	if string(run.data) != expectedData {
		t.Errorf("data = %q, want %q", run.data, expectedData)
	}
	if fb.writeCursor != 36 {
		t.Errorf("writeCursor = %d, want 36", fb.writeCursor)
	}
	if len(fb.articles) != 0 {
		t.Errorf("expected articles to be cleared, got %v", fb.articles)
	}
}

func TestWriteCache_ScratchCoalescing(t *testing.T) {
	wc := newWriteCache(10 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}

	artSize := 200 * 1024
	for i := range 3 {
		bufferAt(wc, key, int64(i*artSize), make([]byte, artSize))
	}
	run1 := wc.flushContiguous(key)
	if run1 == nil {
		t.Fatal("expected run1 to not be nil")
	}
	ptr1 := unsafe.SliceData(run1.data)

	// Buffer another 3 articles and flush again
	for i := range 3 {
		bufferAt(wc, key, int64((i+3)*artSize), make([]byte, artSize))
	}
	run2 := wc.flushContiguous(key)
	if run2 == nil {
		t.Fatal("expected run2 to not be nil")
	}
	ptr2 := unsafe.SliceData(run2.data)

	if ptr1 != ptr2 {
		t.Errorf("expected run2 to reuse wc.scratchBuf backing array (ptr1=%p, ptr2=%p)", ptr1, ptr2)
	}
}

func BenchmarkWriteCache_ContiguousFlush(b *testing.B) {
	wc := newWriteCache(100 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}
	artSize := 200 * 1024
	arts := make([][]byte, 3)
	for i := range arts {
		arts[i] = make([]byte, artSize)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for j := range arts {
			bufferAt(wc, key, int64(j*artSize), arts[j])
		}
		fb := wc.perFile[key]
		fb.writeCursor = 0
		_ = wc.buildContiguousRun(fb, contiguousRunSize)
	}
}

// bufferAt buffers an article without an identity, for the tests whose subject
// is coalescing rather than acking. Tests that care about the ack call
// wc.buffer directly.
func bufferAt(wc *writeCache, key fileKey, offset int64, data []byte) bool {
	cached, _ := wc.buffer(key, bufferedArticle{offset: offset, data: data})
	return cached
}

// TestWriteCacheDiscardAt covers the per-offset drop the displacement path
// needs, including its accounting and its degenerate inputs.
//
// The accounting matters as much as the removal: a discard that forgets to
// return the bytes to wc.used and fb.totalBytes leaks the cache's memory
// budget, which is global across files, so one file's collisions would shrink
// every other file's share until the process restarts.
func TestWriteCacheDiscardAt(t *testing.T) {
	key := fileKey{jobID: "job", fileIdx: 0}

	t.Run("removes the entry and returns its bytes to the budget", func(t *testing.T) {
		wc := newWriteCache(1 << 20)
		if !bufferAt(wc, key, 0, make([]byte, 64)) {
			t.Fatal("precondition: the article was not buffered")
		}
		if !bufferAt(wc, key, 4096, make([]byte, 32)) {
			t.Fatal("precondition: the second article was not buffered")
		}
		if wc.used != 96 {
			t.Fatalf("precondition: used = %d, want 96", wc.used)
		}

		wc.discardAt(key, 0)

		if wc.buffered(key, 0) {
			t.Error("the entry is still buffered, so a later drain writes bytes " +
				"belonging to an article nothing will ack")
		}
		if wc.used != 32 {
			t.Errorf("used = %d, want 32 — the discarded bytes were not returned to "+
				"the global budget", wc.used)
		}
		if got := wc.bytesFor(key); got != 32 {
			t.Errorf("bytesFor = %d, want 32", got)
		}
		if !wc.buffered(key, 4096) {
			t.Error("discardAt removed an entry at a different offset")
		}
	})

	t.Run("an unknown offset is a no-op", func(t *testing.T) {
		wc := newWriteCache(1 << 20)
		bufferAt(wc, key, 0, make([]byte, 64))

		wc.discardAt(key, 4096)

		if !wc.buffered(key, 0) || wc.used != 64 {
			t.Errorf("discarding an absent offset disturbed the cache: buffered=%v used=%d",
				wc.buffered(key, 0), wc.used)
		}
	})

	t.Run("an unknown file is a no-op", func(t *testing.T) {
		wc := newWriteCache(1 << 20)

		wc.discardAt(fileKey{jobID: "nope", fileIdx: 9}, 0)

		// Asserted, not merely survived. Without these the subtest proves only
		// that discardAt does not panic, which no plausible implementation of
		// it would — so a version that lazily created a fileBuf for the missing
		// key, or decremented used below zero, would pass unnoticed.
		if wc.used != 0 {
			t.Errorf("used = %d, want 0", wc.used)
		}
		if len(wc.perFile) != 0 {
			t.Errorf("perFile has %d entries, want 0 — discarding from an unknown "+
				"file created one, so every miss leaks a map entry", len(wc.perFile))
		}
	})
}
