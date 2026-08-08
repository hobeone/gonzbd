package assembler

import "testing"

const drainTestArt = 100 << 10 // 100 KiB, so runs cross contiguousRunSize predictably

// TestWriteCacheDrainKeepsCoalescingAlive is the regression test for #311.
//
// A memory-pressure drain used to delete the file's cache entry outright. The
// next buffered article recreated it with writeCursor back at zero — an offset
// whose article had just been drained and would never be re-buffered — so
// buildContiguousRun broke there on every later call and coalescing was dead
// for the rest of the file. No failed article is involved; this is the
// pressure-flush route to the symptom #311 describes.
func TestWriteCacheDrainKeepsCoalescingAlive(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}
	wc.initCursor(key, 0)

	// Three articles is 300 KiB, below contiguousRunSize, so nothing flushes
	// through the contiguous path and all three are still buffered.
	for i := range 3 {
		wc.buffer(key, int64(i)*drainTestArt, make([]byte, drainTestArt))
	}
	if run := wc.flushContiguous(key); run != nil {
		t.Fatalf("run flushed at %d before the threshold was reached", run.offset)
	}

	// A memory-pressure flush writes everything buffered and hands it back.
	_, drained := wc.drainFile(key)
	if len(drained) != 3 {
		t.Fatalf("drainFile returned %d articles, want 3", len(drained))
	}

	// The rest of the file arrives in order. Coalescing must resume: these
	// articles are contiguous with what was drained, and there are enough of
	// them to clear the threshold.
	for i := 3; i < 12; i++ {
		wc.buffer(key, int64(i)*drainTestArt, make([]byte, drainTestArt))
	}

	run := wc.flushContiguous(key)
	if run == nil {
		t.Fatal("no coalesced run after a pressure drain — the scan is stranded " +
			"at an offset whose article was already written (#311)")
	}
	if want := int64(3 * drainTestArt); run.offset != want {
		t.Errorf("run.offset = %d, want %d (the first offset after the drain)", run.offset, want)
	}
}

// TestWriteCacheDrainAdvancesCursorPastAHole pins the deliberate choice to move
// the cursor past a gap rather than up to it. A drain writes what is buffered
// above a gap as well as below, so stopping at the gap would re-strand the scan
// at the same offset on the next call.
func TestWriteCacheDrainAdvancesCursorPastAHole(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}
	wc.initCursor(key, 0)

	// Articles 0 and 1, then a gap at 2, then article 3.
	wc.buffer(key, 0, make([]byte, drainTestArt))
	wc.buffer(key, drainTestArt, make([]byte, drainTestArt))
	wc.buffer(key, 3*drainTestArt, make([]byte, drainTestArt))

	wc.drainFile(key)

	if got, want := wc.cursorFor(key), int64(4*drainTestArt); got != want {
		t.Errorf("cursorFor = %d, want %d (past the highest drained article, not up to the gap)", got, want)
	}
}

// TestWriteCacheDrainClearsAccounting guards the retained entry: drainFile now
// keeps the fileBuf so its cursor survives, so the byte accounting it holds has
// to be reset explicitly or the cache would believe it is still full and
// force-flush in a loop.
func TestWriteCacheDrainClearsAccounting(t *testing.T) {
	wc := newWriteCache(1 << 20)
	key := fileKey{jobID: "j", fileIdx: 0}
	wc.initCursor(key, 0)

	for i := range 3 {
		wc.buffer(key, int64(i)*drainTestArt, make([]byte, drainTestArt))
	}
	wc.drainFile(key)

	if got := wc.bytesFor(key); got != 0 {
		t.Errorf("bytesFor = %d after drain, want 0", got)
	}
	if wc.used != 0 {
		t.Errorf("wc.used = %d after drain, want 0", wc.used)
	}
	if _, arts := wc.forceFlushLargest(); len(arts) != 0 {
		t.Errorf("forceFlushLargest returned %d articles from a drained cache, want 0", len(arts))
	}
}
