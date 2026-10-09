package assembler

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// Offset collisions (#383, #379).
//
// Two articles claiming one byte offset resolve by whether the incumbent has
// been reported Written. Every accepted article is written synchronously, so an
// incumbent is on disk before a rival can arrive, with one exception: an
// article whose write faulted keeps its acceptedAt entry without having
// written.
//
//   - Incumbent written → the offset is SETTLED and the ARRIVAL is rejected.
//     Its bytes are on disk, the next Drain reports them, and the barrier
//     records the run naming its CRC at that offset and acks it durable.
//     Letting a later article overwrite the range makes that record
//     unverifiable, and failing the incumbent as well would give one article
//     two terminal dispositions. Checked in acceptArticle, refused like any
//     other article-level rejection.
//   - Incumbent faulted, never written → it made no claim, so the arrival
//     takes the offset over.

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

// TestCollision_ArrivalRejectedForZeroLengthIncumbent covers the zero-length
// path: such an article still reaches WriteAt, still owns its offset, and so
// still settles it.
func TestCollision_ArrivalRejectedForZeroLengthIncumbent(t *testing.T) {
	c := newCollisionFixture(t)

	if !c.accept(1, "<empty@x>", 0, nil) {
		t.Fatal("precondition: the zero-length incumbent was not counted")
	}
	c.accept(2, "<second@x>", 0, []byte("BBBB"))

	if len(c.rejected) != 1 || c.rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — a zero-length incumbent still "+
			"reached WriteAt and still owns its offset", c.rejected)
	}
}

// TestCollision_OffsetStaysSettledAfterConfirm is the reason the written flag
// is latched on the offset rather than derived from the barrier's pending
// evidence.
//
// w.written and w.reported are what the barrier has NOT yet finished with;
// Confirm empties both once the articles are acked durable. An article that
// has been acked holds the strongest possible claim on its offset, but a check
// that scanned those slices would see an empty set and read it as no claim —
// so a collision arriving one checkpoint later would overwrite an article the
// queue has already recorded as durably written.
func TestCollision_OffsetStaysSettledAfterConfirm(t *testing.T) {
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

// TestFileWriter_OffsetSettledBy covers the predicate that decides whether an
// arrival is refused, directly, including the ways it must answer "no".
func TestFileWriter_OffsetSettledBy(t *testing.T) {
	owner := articleID{msgID: "owner", artIdx: 1}
	arriving := articleID{msgID: "arriving", artIdx: 2}

	tests := []struct {
		name        string
		seed        func(w *FileWriter)
		arriving    articleID
		wantSettled bool
	}{
		{
			name:        "an unclaimed offset is not settled",
			seed:        func(*FileWriter) {},
			arriving:    arriving,
			wantSettled: false,
		},
		{
			name: "an offset whose owner was never reported Written is not settled",
			seed: func(w *FileWriter) {
				w.acceptedAt[0] = offsetOwner{id: owner}
			},
			arriving:    arriving,
			wantSettled: false,
		},
		{
			name: "an offset whose owner was written is settled",
			seed: func(w *FileWriter) {
				w.acceptedAt[0] = offsetOwner{id: owner, written: true}
			},
			arriving:    arriving,
			wantSettled: true,
		},
		{
			name: "the owner does not settle the offset against ITSELF",
			seed: func(w *FileWriter) {
				w.acceptedAt[0] = offsetOwner{id: owner, written: true}
			},
			// A redelivery of the same article must not be refused as if it
			// were a stranger; handleSuccessArticle's dedup normally catches
			// it first, but a write-fault retry gets past it.
			arriving:    owner,
			wantSettled: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestFileWriter(t)
			tc.seed(w)

			got, settled := w.offsetSettledBy(0, tc.arriving)

			if settled != tc.wantSettled {
				t.Fatalf("settled = %v, want %v", settled, tc.wantSettled)
			}
			if settled && got != owner {
				t.Errorf("owner = %+v, want %+v — the caller names it in the warning",
					got, owner)
			}
		})
	}
}

// TestCollision_ReacceptDoesNotUnsettleAWrittenOffset pins that the written
// latch survives a re-accept by its own owner whose write then FAILS.
//
// Accept records the arrival as the offset's owner. For an article that has
// already been written, re-recording it as a fresh owner resets `written` to
// false, and a re-accept whose write succeeds re-latches it in noteWritten and
// hides the defect. One whose write fails does not: the offset is left
// UNSETTLED over bytes the barrier may already have acked, so the next article
// to claim it is no longer refused and overwrites them — the double
// disposition again.
//
// seenDone is keyed on ArtIdx, so handleSuccessArticle's dedup arm recognises a
// PLAIN redelivery regardless of its Message-ID and returns before
// acceptArticle is called, which is why this calls acceptArticle directly. A
// write fault is what reaches a second Accept: fail deletes the seenDone entry
// and never sets seenFailed, while acceptedAt is never removed by design.
func TestCollision_ReacceptDoesNotUnsettleAWrittenOffset(t *testing.T) {
	c := newCollisionFixture(t)
	id := articleID{msgID: "<first@x>", artIdx: 1}
	req := WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 1, MessageID: "<first@x>", Offset: 0}

	c.f.w.admitAccepted(id.artIdx)
	req.Data = []byte("AAAA")
	if err := c.a.acceptArticle(c.f, id, req); err != nil {
		t.Fatalf("accept incumbent: %v", err)
	}
	if owner := c.f.w.acceptedAt[0]; !owner.written {
		t.Fatal("precondition: the incumbent was not latched as written")
	}

	// Staged as the write-fault retry that is the real route here: fail rolls
	// the part back and clears the seenDone entry, then the redelivery is
	// admitted afresh. Admitting a second time without the fail would charge
	// partsWritten twice for one article.
	c.f.w.fail(id)
	c.f.w.admitAccepted(id.artIdx)
	c.f.w.writeAt = func([]byte, int64) (int, error) { return 0, errors.New("injected write fault") }
	req.Data = []byte("AAAA")
	if err := c.a.acceptArticle(c.f, id, req); err == nil {
		t.Fatal("precondition: the injected write fault did not surface")
	}

	if owner := c.f.w.acceptedAt[0]; !owner.written {
		t.Error("a re-accept by the offset's own owner cleared the written latch, " +
			"unsettling an offset whose bytes are already durable")
	}

	// The consequence: a genuinely different article must still be refused.
	second := articleID{msgID: "<second@x>", artIdx: 2}
	req2 := WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 2, MessageID: "<second@x>", Offset: 0, Data: []byte("BBBB")}
	c.f.w.admitAccepted(second.artIdx)
	if err := c.a.acceptArticle(c.f, second, req2); err != nil {
		c.a.routeAcceptFailure(c.f, req2, err)
	}

	if len(c.rejected) != 1 || c.rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — the offset was unsettled by the "+
			"re-accept, so the arrival overwrote an article the barrier has acked",
			c.rejected)
	}
	// The only article routed to Outstanding is the re-accept whose write
	// failed; the written incumbent's disposition is the barrier's alone.
	if len(c.unwritten) != 0 {
		t.Errorf("OnArticlesUnwritten = %v: acceptArticle does not route, so nothing "+
			"may have reached the callback — the settled arrival is rejected, not rolled back", c.unwritten)
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

// TestFileWriter_RolledBackOwnerKeepsItsOffsetUntilReplaced pins what a write
// fault leaves behind: the article loses its part and its seenDone entry, but
// keeps its acceptedAt entry without having written. The next different
// article to claim the offset replaces it, and once that one is written the
// offset is settled against a third.
func TestFileWriter_RolledBackOwnerKeepsItsOffsetUntilReplaced(t *testing.T) {
	w := newTestFileWriter(t)
	first := articleID{msgID: "first", artIdx: 1}
	second := articleID{msgID: "second", artIdx: 2}
	third := articleID{msgID: "third", artIdx: 3}

	realWrite := w.writeAt
	w.writeAt = func([]byte, int64) (int, error) { return 0, errors.New("injected write fault") }
	w.admitAccepted(first.artIdx)
	if err := w.Accept(first, 0, []byte("AAAA"), 0); err == nil {
		t.Fatal("precondition: the injected write fault did not surface")
	}
	if w.parts() != 0 {
		t.Fatalf("parts = %d after the rollback, want 0", w.parts())
	}
	owner, taken := w.acceptedAt[0]
	if !taken || owner.id != first || owner.written {
		t.Fatalf("acceptedAt[0] = %+v (taken=%v), want the rolled-back article, unwritten", owner, taken)
	}
	if _, settled := w.offsetSettledBy(0, second); settled {
		t.Fatal("an offset owned by a rolled-back article was settled against a rival")
	}

	w.writeAt = realWrite
	w.admitAccepted(second.artIdx)
	if err := w.Accept(second, 0, []byte("BBBB"), 0); err != nil {
		t.Fatalf("accept second: %v", err)
	}
	if owner := w.acceptedAt[0]; owner.id != second || !owner.written {
		t.Errorf("acceptedAt[0] = %+v, want the second article, written", owner)
	}
	if got, settled := w.offsetSettledBy(0, third); !settled || got != second {
		t.Errorf("offsetSettledBy(third) = %+v, %v; want the second article settled", got, settled)
	}
}
