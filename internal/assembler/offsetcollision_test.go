package assembler

import (
	"bytes"
	"errors"
	"testing"
)

// TestFileWriter_DuplicateOffsetAfterFlushIsDetected pins the case the write
// cache's collision detection could not see (#383).
//
// writeCache.buffer used to be the only thing that noticed two articles
// claiming one offset, and it noticed by finding the first still resident in
// fb.articles. buildContiguousRun deletes each article it flushes and advances
// fb.writeCursor past it, so for an IN-ORDER download — the ordinary case — the
// first article is gone from the map long before a duplicate arrives.
//
// The duplicate was then buffered, never picked up by a run (the cursor is
// already past its offset), and written over the first at drain time. Nothing
// was displaced, nothing rolled back, and BOTH articles stayed counted.
//
// The bytes-on-disk outcome is NOT the defect: the arriving article winning the
// offset is the deliberate disposition, here and on the cached path that always
// worked, and whether it is the right one is #382's question. The defect is the
// accounting — a file that reaches TotalParts while one part's bytes were
// silently overwritten by another's, reporting a clean completion.
//
// So this drives admitAccepted, as handleSuccessArticle does, and asserts the
// loser gives its part back.
func TestFileWriter_DuplicateOffsetAfterFlushIsDetected(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(4<<20))

	first := articleID{msgID: "first", artIdx: 1}
	second := articleID{msgID: "second", artIdx: 2}

	// First article, large enough to form a contiguous run from the cursor and
	// flush immediately. contiguousRunSize is 512 KiB.
	w.admitAccepted(first.msgID)
	payload := bytes.Repeat([]byte{'A'}, contiguousRunSize+1)
	if err := w.Accept(first, 0, append([]byte(nil), payload...)); err != nil {
		t.Fatalf("accept first: %v", err)
	}

	// Precondition: it really did flush and leave the cache, which is what made
	// the duplicate below invisible to the old cache-membership check.
	if w.wc.buffered(w.key, 0) {
		t.Fatal("precondition: the first article is still cached, so this test " +
			"would exercise the detected path instead of the undetected one")
	}
	if got := w.parts(); got != 1 {
		t.Fatalf("precondition: parts after first accept = %d, want 1", got)
	}

	// A second article claiming the same offset. In an in-order download this
	// is exactly when a duplicate arrives: after its neighbour has been
	// written.
	w.admitAccepted(second.msgID)
	if err := w.Accept(second, 0, append([]byte(nil), bytes.Repeat([]byte{'B'}, 64)...)); err != nil {
		t.Fatalf("accept second: %v", err)
	}

	rolled := w.takeFaulted()
	if len(rolled) != 1 {
		t.Fatalf("the duplicate offset was not detected: got %d rolled-back articles, want 1 — "+
			"neither article is resolved and the file completes with one part's bytes "+
			"overwritten by the other", len(rolled))
	}
	if rolled[0].id != first {
		t.Errorf("rolled back %+v, want the displaced incumbent %+v", rolled[0].id, first)
	}
	if !rolled[0].displaced {
		t.Error("the rolled-back article is not marked displaced, so routeFaulted will " +
			"return it to Outstanding and the re-fetched copy will displace its displacer")
	}

	// The accounting consequence, which is what makes the silent case corrupt:
	// two articles admitted, one displaced, so exactly one part is held.
	if got := w.parts(); got != 1 {
		t.Errorf("parts = %d, want 1: both articles are still counted, so the file "+
			"can reach TotalParts with only one part's bytes on disk", got)
	}
}

// TestFileWriter_DuplicateOffsetDetectedWithCachingDisabled covers the second
// path the cache-membership check could not see: write_cache_size has no
// validation floor and accepts zero, and Accept then routes every article to
// writeOne without the cache being consulted at all.
func TestFileWriter_DuplicateOffsetDetectedWithCachingDisabled(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(0))

	first := articleID{msgID: "first", artIdx: 1}
	second := articleID{msgID: "second", artIdx: 2}

	w.admitAccepted(first.msgID)
	if err := w.Accept(first, 0, append([]byte(nil), bytes.Repeat([]byte{'A'}, 64)...)); err != nil {
		t.Fatalf("accept first: %v", err)
	}
	w.admitAccepted(second.msgID)
	if err := w.Accept(second, 0, append([]byte(nil), bytes.Repeat([]byte{'B'}, 64)...)); err != nil {
		t.Fatalf("accept second: %v", err)
	}

	rolled := w.takeFaulted()
	if len(rolled) != 1 || rolled[0].id != first {
		t.Fatalf("collision undetected with caching disabled: rolled back %+v, want just %+v",
			rolled, first)
	}
	if got := w.parts(); got != 1 {
		t.Errorf("parts = %d, want 1", got)
	}
}

// TestFileWriter_ReacceptAfterRollbackIsNotACollision is the false-positive
// guard, and the reason detection compares IDENTITY rather than occupancy.
//
// A write fault rolls an article back and returns it to Outstanding. It is
// re-dispatched and comes back at the same offset — req.ArtIdx is the manifest
// index, so the redelivered articleID is identical. Under an occupancy check
// every retry after a transient storage fault would report a collision with
// itself, on a path that is common and where the current code is correct.
func TestFileWriter_ReacceptAfterRollbackIsNotACollision(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(0))
	id := articleID{msgID: "retried", artIdx: 7}

	// First attempt: the write fails, so the article is rolled back.
	w.writeAt = func([]byte, int64) (int, error) { return 0, errors.New("injected write fault") }
	w.admitAccepted(id.msgID)
	if err := w.Accept(id, 0, append([]byte(nil), bytes.Repeat([]byte{'A'}, 64)...)); err == nil {
		t.Fatal("precondition: the injected write fault did not surface")
	}
	if rolled := w.takeFaulted(); len(rolled) != 1 || rolled[0].displaced {
		t.Fatalf("precondition: want one non-displaced rollback, got %+v", rolled)
	}

	// The re-dispatched copy, at the same offset.
	w.writeAt = func(p []byte, _ int64) (int, error) { return len(p), nil }
	w.admitAccepted(id.msgID)
	if err := w.Accept(id, 0, append([]byte(nil), bytes.Repeat([]byte{'A'}, 64)...)); err != nil {
		t.Fatalf("re-accept: %v", err)
	}

	if rolled := w.takeFaulted(); len(rolled) != 0 {
		t.Errorf("the re-accepted article was reported as colliding with itself: %+v", rolled)
	}
}

// TestFileWriter_DuplicateOffsetDetectedForZeroLengthArticle covers the third
// path the cache-membership check could not see. writeCache.buffer refuses a
// zero-length article and returns (false, nil), so it took the writeOne path
// with no collision ever considered — even with caching fully enabled.
func TestFileWriter_DuplicateOffsetDetectedForZeroLengthArticle(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(4<<20))

	first := articleID{msgID: "empty-payload", artIdx: 1}
	second := articleID{msgID: "real-payload", artIdx: 2}

	w.admitAccepted(first.msgID)
	if err := w.Accept(first, 0, nil); err != nil {
		t.Fatalf("accept zero-length: %v", err)
	}
	w.admitAccepted(second.msgID)
	if err := w.Accept(second, 0, append([]byte(nil), bytes.Repeat([]byte{'B'}, 64)...)); err != nil {
		t.Fatalf("accept second: %v", err)
	}

	rolled := w.takeFaulted()
	if len(rolled) != 1 || rolled[0].id != first {
		t.Fatalf("collision undetected for a zero-length incumbent: rolled back %+v, want just %+v",
			rolled, first)
	}
	if got := w.parts(); got != 1 {
		t.Errorf("parts = %d, want 1", got)
	}
}

// TestFileWriter_ThirdArticleAtOneOffsetIsStillDetected pins that detection
// survives its own disposition.
//
// This is the ordering hazard an occupancy-plus-removal design would have hit:
// record the arrival as the new owner, then call failDisplaced on the
// incumbent, and the incumbent's rollback deletes the entry just written — so
// the THIRD article at that offset finds it free and goes undetected, which is
// the original bug reintroduced one article later. Identity comparison never
// removes, so each arrival displaces exactly the previous owner.
func TestFileWriter_ThirdArticleAtOneOffsetIsStillDetected(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(0))

	ids := []articleID{
		{msgID: "a", artIdx: 1},
		{msgID: "b", artIdx: 2},
		{msgID: "c", artIdx: 3},
	}
	for _, id := range ids {
		w.admitAccepted(id.msgID)
		if err := w.Accept(id, 0, append([]byte(nil), bytes.Repeat([]byte{'x'}, 64)...)); err != nil {
			t.Fatalf("accept %s: %v", id.msgID, err)
		}
	}

	rolled := w.takeFaulted()
	if len(rolled) != 2 {
		t.Fatalf("got %d rolled-back articles, want 2 — every article but the last "+
			"owner must be displaced", len(rolled))
	}
	for i, want := range ids[:2] {
		if rolled[i].id != want {
			t.Errorf("rollback %d is %+v, want %+v", i, rolled[i].id, want)
		}
		if !rolled[i].displaced {
			t.Errorf("rollback %d is not marked displaced", i)
		}
	}
	if got := w.parts(); got != 1 {
		t.Errorf("parts = %d, want 1: three articles admitted, two displaced", got)
	}
}

// TestFileWriter_DisplacedUntrackedArticleGivesItsPartBack pins the accounting
// hole in failDisplaced's post-hoc patch-up.
//
// An article with no Message-ID is admitted and counted like any other —
// admitAccepted counts unconditionally, because no map can hold it — but fail
// returns early on an empty ID. So failDisplaced appended nothing, its
// positional `w.faulted[n-1].id == id` guard found no matching entry and
// silently no-opped, and the displaced article kept its part while its
// displacer took another. Two parts counted for one offset, and nothing
// reported the loser rejected.
//
// giveBackUntrackedPart is the only path that can un-admit an untracked
// article, and it is reachable only from routeAcceptFailure — not from
// displacement. Unit 1 is what makes this reachable: it turns collisions from
// timing-dependent into reliably detected.
func TestFileWriter_DisplacedUntrackedArticleGivesItsPartBack(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(0))

	// Two DISTINCT articles, neither carrying a Message-ID. They differ by
	// artIdx, which is what lets identity comparison tell them apart at all.
	first := articleID{msgID: "", artIdx: 1}
	second := articleID{msgID: "", artIdx: 2}

	w.admitAccepted(first.msgID)
	if err := w.Accept(first, 0, append([]byte(nil), bytes.Repeat([]byte{'A'}, 64)...)); err != nil {
		t.Fatalf("accept first: %v", err)
	}
	w.admitAccepted(second.msgID)
	if err := w.Accept(second, 0, append([]byte(nil), bytes.Repeat([]byte{'B'}, 64)...)); err != nil {
		t.Fatalf("accept second: %v", err)
	}

	rolled := w.takeFaulted()
	if len(rolled) != 1 {
		t.Fatalf("got %d rolled-back articles, want 1: an untracked article's "+
			"displacement was dropped entirely, so nothing resolves it", len(rolled))
	}
	if rolled[0].id != first {
		t.Errorf("rolled back %+v, want the displaced incumbent %+v", rolled[0].id, first)
	}
	if !rolled[0].displaced {
		t.Error("the rolled-back untracked article is not marked displaced")
	}
	if got := w.parts(); got != 1 {
		t.Errorf("parts = %d, want 1: the displaced untracked article kept its part "+
			"while its displacer took another, so the file can reach TotalParts "+
			"with one offset's bytes counted twice", got)
	}
}
