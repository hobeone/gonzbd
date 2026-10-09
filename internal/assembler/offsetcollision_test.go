package assembler

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// Offset collisions (#383, #379).
//
// Two articles claiming overlapping byte ranges resolve by whether the incumbent
// has written. Every accepted article is written synchronously, so an incumbent
// is on disk before a rival can arrive, with one exception: an article whose
// write faulted owns nothing.
//
//   - Incumbent written → the range is OWNED and the ARRIVAL is rejected.
//     Its bytes are on disk, the next Drain reports them, and the barrier
//     records the run naming its CRC at that offset and acks it durable.
//     Letting a later article overwrite the range makes that record
//     unverifiable, and failing the incumbent as well would give one article
//     two terminal dispositions. Checked in acceptArticle, refused like any
//     other article-level rejection.
//   - Incumbent faulted, never written → it claimed nothing, so the arrival is
//     accepted.

// --- Settled offsets: the arrival is rejected ------------------------------

// collisionFixture drives two articles at one offset through the real accept
// path and reports what came out.
type collisionFixture struct {
	a        *Assembler
	f        *openFile
	rejected []int32
	// unwritten collects what OnArticlesUnwritten was told to return to
	// Outstanding, which is how a rolled-back article is observed.
	unwritten []int32
}

func newCollisionFixture(t *testing.T) *collisionFixture {
	t.Helper()
	c := &collisionFixture{a: newHelperAssembler()}
	c.a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		c.rejected = append(c.rejected, artIdx)
	}
	c.a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		c.unwritten = append(c.unwritten, arts...)
	}
	c.f = newHelperFile(t, t.TempDir(), "collide.dat", 1<<20)
	c.f.info.TotalParts = 2
	return c
}

func (c *collisionFixture) accept(artIdx int32, msgID string, off int64, data []byte) bool {
	return c.a.handleSuccessArticle(c.f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: artIdx, MessageID: msgID,
		Offset: off, Data: data,
	})
}

// TestCollision_ArrivalRejectedOnceIncumbentIsWritten is the #383 pin: an
// arrival at an offset whose incumbent has been written is refused, and the
// incumbent's bytes stay the truth at that offset.
func TestCollision_ArrivalRejectedOnceIncumbentIsWritten(t *testing.T) {
	c := newCollisionFixture(t)

	if !c.accept(1, "<first@x>", 0, []byte("AAAA")) {
		t.Fatal("precondition: the incumbent was not counted")
	}
	counted := c.accept(2, "<second@x>", 0, []byte("BBBB"))

	if !counted {
		t.Error("the rejected arrival was not counted toward the file's part total")
	}
	if len(c.rejected) != 1 || c.rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — the arrival loses a written offset", c.rejected)
	}
	if len(c.unwritten) != 0 {
		t.Errorf("the written incumbent was returned to Outstanding as well as remaining in the "+
			"barrier's evidence: %v — one article, two terminal dispositions", c.unwritten)
	}
	onDisk, err := os.ReadFile(c.f.w.path)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if len(onDisk) < 4 || string(onDisk[:4]) != "AAAA" {
		t.Errorf("bytes at offset 0 = %q, want the incumbent's AAAA", onDisk)
	}
}

// TestCollision_RangeStaysOwnedAfterConfirm is the reason the written flag
// is recorded on the range rather than derived from the barrier's pending
// evidence.
//
// w.written and w.reported are what the barrier has NOT yet finished with;
// Confirm empties both once the articles are acked durable. An article that
// has been acked holds the strongest possible claim on its offset, but a check
// that scanned those slices would see an empty set and read it as no claim —
// so a collision arriving one checkpoint later would overwrite an article the
// queue has already recorded as durably written.
func TestCollision_RangeStaysOwnedAfterConfirm(t *testing.T) {
	c := newCollisionFixture(t)

	if !c.accept(1, "<first@x>", 0, []byte("AAAA")) {
		t.Fatal("precondition: the incumbent was not counted")
	}
	// Complete a full barrier cycle, which is what empties the evidence.
	if _, err := c.f.w.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	c.f.w.Confirm()
	if len(c.f.w.writtenSoFar()) != 0 || len(c.f.w.unconfirmed()) != 0 {
		t.Fatalf("precondition: the barrier's evidence is not empty (written=%d "+
			"reported=%d), so this test cannot distinguish a latched claim from a "+
			"derived one", len(c.f.w.writtenSoFar()), len(c.f.w.unconfirmed()))
	}

	c.accept(2, "<second@x>", 0, []byte("BBBB"))

	if len(c.rejected) != 1 || c.rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — after Confirm the incumbent has "+
			"been ACKED durable, and overwriting it now contradicts a fact the queue "+
			"has already recorded", c.rejected)
	}
	if len(c.unwritten) != 0 {
		t.Errorf("an already-acked article was returned to Outstanding: %v", c.unwritten)
	}
}

// TestFileWriter_RangeOwnedBy covers the predicate that decides whether an
// arrival is refused, directly, including the ways it must answer "no".
func TestFileWriter_RangeOwnedBy(t *testing.T) {
	owner := articleID{msgID: "owner", artIdx: 1}
	arriving := articleID{msgID: "arriving", artIdx: 2}

	tests := []struct {
		name      string
		seed      func(w *FileWriter)
		arriving  articleID
		r         Range
		wantOwned bool
	}{
		{"an unclaimed range is not owned", func(*FileWriter) {}, arriving, Range{0, 4}, false},
		{"the same range is owned", func(w *FileWriter) { w.owned.claim(Range{0, 4}, owner) }, arriving, Range{0, 4}, true},
		{"a partial overlap is owned", func(w *FileWriter) { w.owned.claim(Range{0, 4}, owner) }, arriving, Range{2, 4}, true},
		{"an abutting range is not owned", func(w *FileWriter) { w.owned.claim(Range{0, 4}, owner) }, arriving, Range{4, 4}, false},
		{"the owner does not collide with ITSELF", func(w *FileWriter) { w.owned.claim(Range{0, 4}, owner) }, owner, Range{0, 4}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestFileWriter(t)
			tc.seed(w)

			got, owned := w.rangeOwnedBy(tc.r, tc.arriving)

			if owned != tc.wantOwned {
				t.Fatalf("owned = %v, want %v", owned, tc.wantOwned)
			}
			if owned && got != owner {
				t.Errorf("owner = %+v, want %+v — the caller names it in the warning", got, owner)
			}
		})
	}
}

// TestCollision_FailedReacceptKeepsTheWrittenRange pins that the range survives
// a re-accept by its own owner whose write then FAILS: the owner keeps the
// range it wrote, so the next article to claim it is still refused.
//
// seenDone is keyed on ArtIdx, so handleSuccessArticle's dedup arm recognises a
// PLAIN redelivery and returns before acceptArticle is called, which is why this
// calls acceptArticle directly. A write fault is what reaches a second Accept:
// fail deletes the seenDone entry and never sets seenFailed.
func TestCollision_FailedReacceptKeepsTheWrittenRange(t *testing.T) {
	c := newCollisionFixture(t)
	id := articleID{msgID: "<first@x>", artIdx: 1}
	req := WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 1, MessageID: "<first@x>", Offset: 0}

	c.f.w.admitAccepted(id.artIdx)
	req.Data = []byte("AAAA")
	if err := c.a.acceptArticle(c.f, id, req); err != nil {
		t.Fatalf("accept incumbent: %v", err)
	}
	if owner, owned := c.f.w.owned.ownerOf(Range{0, 4}, articleID{artIdx: 99}); !owned || owner != id {
		t.Fatalf("precondition: ownerOf = %+v, %v; want the incumbent", owner, owned)
	}

	c.f.w.fail(id)
	c.f.w.admitAccepted(id.artIdx)
	c.f.w.writeAt = func([]byte, int64) (int, error) { return 0, errors.New("injected write fault") }
	req.Data = []byte("AAAA")
	if err := c.a.acceptArticle(c.f, id, req); err == nil {
		t.Fatal("precondition: the injected write fault did not surface")
	}

	second := articleID{msgID: "<second@x>", artIdx: 2}
	req2 := WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 2, MessageID: "<second@x>", Offset: 0, Data: []byte("BBBB")}
	c.f.w.admitAccepted(second.artIdx)
	if err := c.a.acceptArticle(c.f, second, req2); err != nil {
		c.a.routeAcceptFailure(c.f, req2, err)
	}

	if len(c.rejected) != 1 || c.rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — the failed re-accept released the "+
			"range, so the arrival overwrote an article the barrier has acked", c.rejected)
	}
	if len(c.unwritten) != 0 {
		t.Errorf("OnArticlesUnwritten = %v: acceptArticle does not route, so nothing "+
			"may have reached the callback", c.unwritten)
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
	w := newTestFileWriter(t)
	id := articleID{msgID: "retried", artIdx: 7}

	w.writeAt = func([]byte, int64) (int, error) { return 0, errors.New("injected write fault") }
	w.admitAccepted(id.artIdx)
	if err := w.Accept(id, 0, append([]byte(nil), bytes.Repeat([]byte{'A'}, 64)...), 0); err == nil {
		t.Fatal("precondition: the injected write fault did not surface")
	}
	if w.parts() != 0 {
		t.Fatalf("precondition: parts = %d after the rollback, want 0", w.parts())
	}

	// The re-dispatched copy, at the same offset.
	w.writeAt = func(p []byte, _ int64) (int, error) { return len(p), nil }
	w.admitAccepted(id.artIdx)
	if err := w.Accept(id, 0, append([]byte(nil), bytes.Repeat([]byte{'A'}, 64)...), 0); err != nil {
		t.Fatalf("re-accept: %v", err)
	}

	if got := w.parts(); got != 1 {
		t.Errorf("parts = %d after the re-accept, want 1: it was treated as a collision with itself", got)
	}
}

// TestFileWriter_FaultedWriteOwnsNothing pins what a write fault leaves
// behind: the article loses its part and its seenDone entry and, because its
// range is claimed only after the write returned nil, owns nothing. A different
// article at the same range is accepted, and once written it owns the range
// against a third.
func TestFileWriter_FaultedWriteOwnsNothing(t *testing.T) {
	w := newTestFileWriter(t)
	first := articleID{msgID: "first", artIdx: 1}
	second := articleID{msgID: "second", artIdx: 2}
	third := articleID{msgID: "third", artIdx: 3}
	r := Range{0, 4}

	realWrite := w.writeAt
	w.writeAt = func([]byte, int64) (int, error) { return 0, errors.New("injected write fault") }
	w.admitAccepted(first.artIdx)
	if err := w.Accept(first, 0, []byte("AAAA"), 0); err == nil {
		t.Fatal("precondition: the injected write fault did not surface")
	}
	if w.parts() != 0 {
		t.Fatalf("parts = %d after the rollback, want 0", w.parts())
	}
	if owner, owned := w.owned.ownerOf(r, second); owned {
		t.Fatalf("ownerOf = %+v after a faulted write, want no owner", owner)
	}
	if _, owned := w.rangeOwnedBy(r, second); owned {
		t.Fatal("a range whose only writer faulted was owned against a rival")
	}

	w.writeAt = realWrite
	w.admitAccepted(second.artIdx)
	if err := w.Accept(second, 0, []byte("BBBB"), 0); err != nil {
		t.Fatalf("accept second: %v", err)
	}
	if got, owned := w.rangeOwnedBy(r, third); !owned || got != second {
		t.Errorf("rangeOwnedBy(third) = %+v, %v; want the second article to own it", got, owned)
	}
}
