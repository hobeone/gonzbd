package assembler

import (
	"testing"
)

// TestDisplacedArticle_IsCountedAndResolved pins the disposition of the article
// that LOSES an offset collision: it is counted toward the file's part total,
// resolved permanently failed, and never returned to Outstanding.
//
// Two segments resolving to one offset is not exotic. The offset comes from the
// server's yEnc =ypart header rather than from the NZB, so a mispost or a faulty
// server can put two distinct segments at one byte position, and each passes
// validation on its own because nothing checks an article against its siblings.
//
// Article X is admitted and counted. Article Y arrives at the same offset;
// Accept's acceptedAt check finds X, discards its buffered bytes through
// wc.discardAt, and routes X to failDisplaced. X keeps its count because it is
// RESOLVED — OnArticleRejected carries it to the queue and it will never arrive
// again, so a file that stopped counting it could never reach TotalParts.
//
// The name this test carried until 2026-08-19 asserted the opposite — that X
// gives its part back. That WAS the behaviour, and returning X to Outstanding
// is what produced the [0 1 0 1 0] ping-pong the last assertion still names.
// b2793dc1 (#386) changed it and rewrote the assertions below without rewriting
// the name or this comment, so both went on documenting a defect that no longer
// existed. #379 was filed against that stale reading and outlived its own fix.
//
// Driven through processRequest rather than handleSuccessArticle, because
// processRequest owns the drain that establishes "w.faulted is empty when
// partsWritten is compared to TotalParts". A test calling the inner function
// would pass with the drain deleted.
func TestDisplacedArticle_IsCountedAndResolved(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	var unwritten []int32
	var rejected []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		unwritten = append(unwritten, artIdxs...)
	}
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}

	wc := newWriteCache(1 << 20)
	f := newHelperFile(t, dir, "displaced.dat", 0)
	f.w.wc = wc
	f.info.TotalParts = 4
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	for _, art := range []struct {
		msg string
		idx int32
	}{{"x", 0}, {"y", 1}} {
		a.processRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: art.idx, MessageID: art.msg,
			Offset: 0, Data: []byte("XXXX"),
		}, open, completed, wc)
	}

	// Fixture check on a signal the code under test does not produce: X's
	// bytes must actually have been displaced from the cache. Asserting on
	// the rollback instead would make the fixture guard the pin, so removing
	// the fix would fail here with "the fixture did not displace X" — sending
	// a maintainer to the wrong mechanism.
	if got := wc.bytesFor(key); got != 4 {
		t.Fatalf("cache holds %d bytes for the file, want 4 (one article) — the fixture "+
			"did not displace X, so it proves nothing about the displaced-article path",
			got)
	}

	if f.w.parts() != 2 {
		t.Errorf("partsWritten = %d, want 2 — X was displaced and its bytes pooled, but "+
			"TotalParts counts X and Y as two segments, so both must be accounted for. "+
			"X is counted as permanently failed rather than written; declining to count "+
			"it leaves the file waiting on a part nothing can supply (#386)", f.w.parts())
	}
	if len(rejected) != 1 || rejected[0] != 0 {
		t.Errorf("rejected = %v, want [0] — a displaced article must be RESOLVED, not "+
			"re-fetched: the collision is a property of what the server sent, so the "+
			"re-fetched copy displaces the article that displaced it", rejected)
	}
	if len(unwritten) != 0 {
		t.Errorf("unwritten = %v, want none — returning a displaced article to "+
			"Outstanding is what produced the [0 1 0 1 0] ping-pong", unwritten)
	}
}
