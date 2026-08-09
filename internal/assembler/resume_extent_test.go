package assembler

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// resumeArt is one article's decoded payload size in these tests. Small — none
// of this exercises the coalescing threshold, only the extent bookkeeping.
const resumeArt = 4

// TestResumeKeepsEarlierRunBytes is the regression test for #342.
//
// finalizeFile truncates a completed file to openFile.maxWritten, a high-water
// mark of bytes written during the current run. A resumed file's TotalParts
// counts only the articles still outstanding, so it completes as soon as those
// arrive. When they sit below what an earlier run already wrote, the truncate
// cuts the tail off a file that was correct on disk.
//
// The target file is opened O_CREATE without O_TRUNC precisely so an earlier
// run's bytes survive the open — this test asserts they also survive the close.
func TestResumeKeepsEarlierRunBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job1_0.dat")

	// An earlier run wrote 20 bytes and was interrupted before completion.
	prior := []byte("0123456789ABCDEFGHIJ")
	if err := os.WriteFile(path, prior, 0o644); err != nil {
		t.Fatalf("seed prior run's file: %v", err)
	}

	files := map[string]FileInfo{
		"job1:0": {
			Path:       path,
			TotalParts: 1, // only the one still-unfinished article
			// The earlier run persisted its progress: 20 bytes reached disk.
			InitialMaxWritten: int64(len(prior)),
		},
	}
	opts := makeOpts(dir, files)
	done := make(chan struct{}, 1)
	opts.OnFileComplete = func(string, int, uint32) { done <- struct{}{} }

	a := startAssembler(t, opts)
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, Offset: 0, Data: []byte("ZZZZ"),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	<-done
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	got := readFile(t, path)
	if len(got) < len(prior) {
		t.Errorf("file shrank to %d bytes, want at least %d — the earlier run's "+
			"tail was truncated away (#342)", len(got), len(prior))
	}
	if want := "ZZZZ456789ABCDEFGHIJ"; string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

// TestWriteCursorSeedsTheExtent covers the same defect for a job whose earlier
// run predates the persisted high-water mark, or whose mark is unset for any
// other reason. InitialWriteCursor is the contiguous frontier of bytes that
// reached disk, so it is always at or below the true extent and can serve as a
// floor that never over-truncates.
func TestWriteCursorSeedsTheExtent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job1_0.dat")

	prior := []byte("0123456789ABCDEFGHIJ")
	if err := os.WriteFile(path, prior, 0o644); err != nil {
		t.Fatalf("seed prior run's file: %v", err)
	}

	files := map[string]FileInfo{
		"job1:0": {
			Path:       path,
			TotalParts: 1,
			// No InitialMaxWritten — only the older cursor hint.
			InitialWriteCursor: int64(len(prior)),
		},
	}
	opts := makeOpts(dir, files)
	done := make(chan struct{}, 1)
	opts.OnFileComplete = func(string, int, uint32) { done <- struct{}{} }

	a := startAssembler(t, opts)
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, Offset: 0, Data: []byte("ZZZZ"),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	<-done
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := readFile(t, path); len(got) < len(prior) {
		t.Errorf("file shrank to %d bytes, want at least %d — the write cursor "+
			"is a floor on the true extent and must not be truncated below",
			len(got), len(prior))
	}
}

// TestExtentReportedWithCacheDisabled pins that the resume figures are
// persisted on the uncached write path too.
//
// pendingCursor was recorded in exactly one place, inside the branch taken when
// an article is buffered. With WriteCacheBytes at 0 — a supported setting — the
// direct-write path returns without recording anything, so nothing was ever
// persisted and a restart resumed from zero. That silently disabled the resume
// hint rather than degrading it.
func TestExtentReportedWithCacheDisabled(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 2)

	type extent struct{ cursor, maxWritten int64 }
	reported := make(map[int]extent)

	var mu sync.Mutex
	opts := makeOpts(dir, files)
	opts.WriteCacheBytes = 0 // caching off — exercise the direct-write path
	opts.DoneFlushInterval = 10 * time.Millisecond
	opts.SetFileExtents = func(_ string, fileIdx int, cursor, maxWritten int64) error {
		mu.Lock()
		defer mu.Unlock()
		reported[fileIdx] = extent{cursor: cursor, maxWritten: maxWritten}
		return nil
	}

	a := startAssembler(t, opts)
	// One of the file's two parts. The file stays incomplete deliberately:
	// finalizeFile drops the pending entry before the flush, since a finished
	// file has no resume state worth persisting, so a completed file reports
	// nothing by design.
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, Offset: 0, Data: []byte("abcd"),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		_, ok := reported[0]
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	got, ok := reported[0]
	mu.Unlock()
	if !ok {
		t.Fatal("no extent reported with the write cache disabled — the resume " +
			"figures are silently unpersisted in this configuration, so a restart " +
			"resumes from zero")
	}
	if got.maxWritten != resumeArt {
		t.Errorf("reported maxWritten = %d, want %d", got.maxWritten, int64(resumeArt))
	}
	if st, err := os.Stat(path); err != nil || st.Size() != resumeArt {
		t.Errorf("file on disk: %v (err %v); the reported mark must describe bytes "+
			"that actually reached disk", st, err)
	}
}

// TestCloseJobHandlesPersistsTheFinalExtent pins the retry path.
//
// CloseJobHandles runs when a job enters post-processing, and the files still
// open at that moment are the incomplete ones — the ones a retry resumes. It
// drains the write cache first, which writes the buffered tail and raises the
// high-water mark, so that final drain is exactly the increment a retry needs.
// Dropping the pending entry before the flush discarded it, leaving the
// database describing the file as it stood at the last periodic flush and
// re-arming #342 on the one path where the file is guaranteed partial.
func TestCloseJobHandlesPersistsTheFinalExtent(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	// Four parts expected, two delivered: the file stays incomplete, as it
	// would be for a job entering post-processing with failed articles.
	registerFile(t, dir, files, "job1", 0, 4)

	var mu sync.Mutex
	var maxReported int64
	opts := makeOpts(dir, files)
	opts.WriteCacheBytes = 64 << 20 // buffered, so only the drain writes them
	opts.SetFileExtents = func(_ string, _ int, _, maxWritten int64) error {
		mu.Lock()
		defer mu.Unlock()
		maxReported = max(maxReported, maxWritten)
		return nil
	}

	a := startAssembler(t, opts)
	for i := range 2 {
		if err := a.WriteArticle(t.Context(), WriteRequest{
			JobID: "job1", FileIdx: 0,
			Offset: int64(i) * resumeArt,
			Data:   []byte("abcd"),
		}); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}

	if err := a.CloseJobHandles(t.Context(), "job1"); err != nil {
		t.Fatalf("CloseJobHandles: %v", err)
	}

	mu.Lock()
	got := maxReported
	mu.Unlock()

	if want := int64(2 * resumeArt); got != want {
		t.Errorf("persisted maxWritten = %d, want %d — the drain wrote the tail "+
			"and raised the mark, but the entry was dropped before the flush, so "+
			"a retry resumes from a stale extent and truncates those bytes away",
			got, want)
	}
}

// TestExtentCountsOnlyWrittenBytes pins that the persisted high-water mark
// describes bytes that reached WriteAt, not bytes sitting in the write cache.
//
// The mark was raised before the article was buffered, while the same path
// acked the article Done. A crash after that flush persisted a mark above the
// file's real extent with its articles recorded complete, so they were never
// refetched — and seeding a resumed file from such a mark would make the
// truncate extend a short file with zeros.
func TestExtentCountsOnlyWrittenBytes(t *testing.T) {
	const art = 128 << 10 // 128 KiB; four of these clear contiguousRunSize

	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 64)

	var mu sync.Mutex
	var calls int
	var maxReported int64
	opts := makeOpts(dir, files)
	opts.WriteCacheBytes = 64 << 20
	opts.DoneFlushInterval = 10 * time.Millisecond
	opts.SetFileExtents = func(_ string, _ int, _, maxWritten int64) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		maxReported = max(maxReported, maxWritten)
		return nil
	}

	a := startAssembler(t, opts)

	// Five contiguous articles from zero: enough to clear the coalescing
	// threshold, so these are written to disk and the mark legitimately covers
	// them.
	for i := range 5 {
		if err := a.WriteArticle(t.Context(), WriteRequest{
			JobID: "job1", FileIdx: 0,
			Offset: int64(i) * art,
			Data:   make([]byte, art),
		}); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}

	// One article far past a gap. It cannot join a contiguous run, so it stays
	// buffered and unwritten — but it is acked Done all the same, and before
	// the fix it raised the mark to its own end at buffer time.
	const farOffset = 40 * art
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, Offset: farOffset, Data: make([]byte, art),
	}); err != nil {
		t.Fatalf("WriteArticle far: %v", err)
	}

	// More contiguous articles, to drive a second coalesced flush. This is what
	// makes the test able to fail: the mark is only reported when a write
	// happens, so without a write after the far article the inflated value
	// would sit in memory unreported and the assertion would hold vacuously.
	for i := 5; i < 9; i++ {
		if err := a.WriteArticle(t.Context(), WriteRequest{
			JobID: "job1", FileIdx: 0,
			Offset: int64(i) * art,
			Data:   make([]byte, art),
		}); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	gotCalls, reported := calls, maxReported
	mu.Unlock()

	if gotCalls == 0 {
		t.Fatal("the extent was never reported, so this test proves nothing about " +
			"what it would have contained")
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if reported > st.Size() {
		t.Errorf("persisted maxWritten = %d with only %d bytes on disk — the "+
			"buffered article past the gap raised the mark before it was written. "+
			"A crash here resumes from a mark past the file's real extent, and "+
			"that article is already acked done so it is never refetched",
			reported, st.Size())
	}
	// Writes here are contiguous from zero, so the mark should land exactly on
	// the file's size rather than merely at or below it. Not asserting against
	// a fixed constant: the coalescer flushes when a run clears its threshold,
	// so how many of the five articles are on disk at this point is its
	// business, not this test's.
	if reported != st.Size() {
		t.Errorf("persisted maxWritten = %d but the file is %d bytes; the mark "+
			"should track exactly what reached disk", reported, st.Size())
	}
	if st.Size() >= farOffset {
		t.Fatalf("the far article was written after all (file is %d bytes); this "+
			"test no longer distinguishes buffered from written", st.Size())
	}
}
