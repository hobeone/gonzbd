package assembler

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// Offset collisions (#383, #379).
//
// Two articles claiming one byte offset resolve one of two ways, and WHICH
// depends on whether the incumbent has been reported Written:
//
//   - Incumbent written → the offset is SETTLED and the ARRIVAL is rejected.
//     Its bytes are on disk, the pipeline has recorded a Class A fact naming
//     its CRC at that offset, and the barrier will ack it durable. Letting a
//     later article overwrite the range makes that fact unverifiable, and
//     failing the incumbent as well would give one article two terminal
//     dispositions. Checked in acceptArticle, refused like any other
//     article-level rejection.
//   - Incumbent still cached, unwritten → the INCUMBENT is displaced, which is
//     what the write cache always did. It has made no claim, so nothing is
//     contradicted.
//
// The tests below are split on that line, because the layer differs too: a
// rejection is the assembler's to make and a displacement is the writer's.

// --- Settled offsets: the arrival is rejected ------------------------------

// collisionFixture drives two articles at one offset through the real accept
// path and reports what came out.
type collisionFixture struct {
	a         *Assembler
	f         *openFile
	rejected  []int32
	anomalies []string
}

func newCollisionFixture(t *testing.T, cacheBytes int64) *collisionFixture {
	t.Helper()
	c := &collisionFixture{a: newHelperAssembler()}
	c.a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		c.rejected = append(c.rejected, artIdx)
	}
	c.a.opts.OnPostAnomaly = func(_ string, _ int, reason string) {
		c.anomalies = append(c.anomalies, reason)
	}
	c.f = newHelperFile(t, t.TempDir(), "collide.dat", 1<<20)
	c.f.info.TotalParts = 2
	c.f.w.wc = newWriteCache(cacheBytes)
	return c
}

func (c *collisionFixture) accept(artIdx int32, msgID string, off int64, data []byte) bool {
	return c.a.handleSuccessArticle(c.f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: artIdx, MessageID: msgID,
		Offset: off, Data: data,
	})
}

// TestCollision_ArrivalRejectedOnceIncumbentIsWritten is the #383 pin, in the
// case that made it: an IN-ORDER download, where the incumbent was flushed and
// evicted from the write cache long before the duplicate arrived.
//
// The old detector keyed on cache residency, so it saw nothing here: the
// duplicate was buffered, never picked up by a run, and written over the first
// at drain time. No detection, no roll-back, both counted, file completes.
func TestCollision_ArrivalRejectedOnceIncumbentIsWritten(t *testing.T) {
	c := newCollisionFixture(t, 4<<20)

	// Large enough to form a contiguous run from the cursor and flush at once.
	big := bytes.Repeat([]byte{'A'}, contiguousRunSize+1)
	if !c.accept(1, "<first@x>", 0, big) {
		t.Fatal("precondition: the incumbent was not counted")
	}
	if c.f.w.wc.buffered(c.f.w.key, 0) {
		t.Fatal("precondition: the incumbent is still cached, so this exercises the " +
			"displacement path instead of the settled one")
	}
	if len(c.f.w.writtenSoFar()) == 0 {
		t.Fatal("precondition: the incumbent was not reported Written")
	}

	counted := c.accept(2, "<second@x>", 0, []byte("BBBB"))

	if !counted {
		t.Error("the rejected arrival was not counted toward the file's part total; it " +
			"is resolved permanently failed and never arrives again, so the file could " +
			"never reach TotalParts")
	}
	if len(c.rejected) != 1 || c.rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — the ARRIVAL loses a settled offset",
			c.rejected)
	}
	if got := c.f.w.takeFaulted(); len(got) != 0 {
		t.Errorf("the written incumbent was rolled back as well as remaining in the "+
			"barrier's evidence: %+v — one article, two terminal dispositions", got)
	}
	if len(c.anomalies) != 1 {
		t.Errorf("OnPostAnomaly fired %d times, want 1 — the collision is still the "+
			"thing the user has to be told about", len(c.anomalies))
	}

	// The incumbent's bytes are still the truth at that offset, which is what
	// keeps its Class A fact verifiable after a restart.
	if _, err := c.f.w.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	onDisk, err := os.ReadFile(c.f.w.path)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if len(onDisk) == 0 || onDisk[0] != 'A' {
		t.Error("the rejected article's bytes were written anyway, so the fact log's " +
			"CRC for the incumbent no longer describes the range it names")
	}
}

// TestCollision_ArrivalRejectedWithCachingDisabled covers the second path the
// old cache-membership check could not see. writeCache.enabled() is limit > 0
// and downloads.write_cache_size has no validation floor, so zero disables the
// cache and every article goes straight to writeOne.
func TestCollision_ArrivalRejectedWithCachingDisabled(t *testing.T) {
	c := newCollisionFixture(t, 0)

	if !c.accept(1, "<first@x>", 0, []byte("AAAA")) {
		t.Fatal("precondition: the incumbent was not counted")
	}
	c.accept(2, "<second@x>", 0, []byte("BBBB"))

	if len(c.rejected) != 1 || c.rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — with caching disabled the old "+
			"detector was never consulted at all", c.rejected)
	}
	if got := c.f.w.takeFaulted(); len(got) != 0 {
		t.Errorf("the written incumbent was rolled back: %+v", got)
	}
}

// TestCollision_ArrivalRejectedForZeroLengthIncumbent covers the third path.
// writeCache.buffer refuses a zero-length article and returns (false, nil), so
// it took writeOne with no collision considered — even with caching enabled.
func TestCollision_ArrivalRejectedForZeroLengthIncumbent(t *testing.T) {
	c := newCollisionFixture(t, 4<<20)

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
// so a collision arriving one checkpoint later would displace an article the
// queue has already recorded as durably written.
func TestCollision_OffsetStaysSettledAfterConfirm(t *testing.T) {
	c := newCollisionFixture(t, 0)

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
			"been ACKED durable, and displacing it now contradicts a fact the queue "+
			"has already recorded", c.rejected)
	}
	if got := c.f.w.takeFaulted(); len(got) != 0 {
		t.Errorf("an already-acked article was rolled back: %+v", got)
	}
}

// TestCollision_CachedIncumbentIsDisplacedNotRejected pins the OTHER side of
// the settled test, through the same path, which is what makes either of them
// mean anything.
//
// The two dispositions are chosen by one condition, and a check that ignored
// it would still satisfy every settled-offset assertion above by rejecting
// everything. This is the test that fails when it does: an incumbent still
// buffered has made no claim, so the arrival must WIN and the incumbent must
// be rolled back.
func TestCollision_CachedIncumbentIsDisplacedNotRejected(t *testing.T) {
	c := newCollisionFixture(t, 1<<20)

	// Small enough that no contiguous run forms, so it stays buffered.
	if !c.accept(1, "<first@x>", 0, []byte("AAAA")) {
		t.Fatal("precondition: the incumbent was not counted")
	}
	if len(c.f.w.writtenSoFar()) != 0 {
		t.Fatal("precondition: the incumbent was reported Written, so this exercises " +
			"the settled path and cannot discriminate the condition")
	}

	if !c.accept(2, "<second@x>", 0, []byte("BBBB")) {
		t.Error("the arriving article was not counted")
	}

	// The incumbent loses, and it loses by DISPLACEMENT — a faulted record —
	// not by the arrival being refused.
	if len(c.rejected) != 0 {
		t.Errorf("OnArticleRejected = %v, want none at accept time: an unwritten "+
			"incumbent contradicts nothing, so refusing the arrival here would fail "+
			"a healthy article and leave stale bytes owning the offset", c.rejected)
	}
	rolled := c.f.w.takeFaulted()
	if len(rolled) != 1 || rolled[0].id.artIdx != 1 || !rolled[0].displaced {
		t.Fatalf("faulted = %+v, want the incumbent (artIdx 1) marked displaced", rolled)
	}
}

// TestFileWriter_OffsetSettledBy covers the predicate that chooses between the
// two dispositions, directly, including the two ways it must answer "no".
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
			name: "an offset whose owner is only buffered is not settled",
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
			// it first, but an article with no Message-ID cannot be deduped.
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

// TestFileWriter_NotePostAnomalyLatches pins the latch both dispositions share,
// so a file raises one job-level warning however its collisions resolve.
func TestFileWriter_NotePostAnomalyLatches(t *testing.T) {
	w := newTestFileWriter(t)

	if !w.notePostAnomaly() {
		t.Fatal("the first collision on a file did not claim the warning, so none is raised")
	}
	if w.notePostAnomaly() {
		t.Error("a second collision claimed the warning again, which is what overwrites " +
			"job.Warning once per colliding segment")
	}
}

// TestCollision_ReacceptDoesNotUnsettleAWrittenOffset pins that the written
// latch survives a re-accept by its own owner.
//
// Accept records the arrival as the offset's owner unconditionally. For an
// article that has already been written, re-recording it as a fresh owner
// resets `written` to false and UNSETTLES the offset — after which the next
// article to claim it is no longer refused, and displaces an article whose
// bytes the barrier has already acked. That is the double-disposition defect
// again, reached through the one article kind that can re-enter Accept at all.
//
// Only an article with no Message-ID can: handleSuccessArticle's dedup arms
// key on seenDone/seenFailed, and admitAccepted records nothing for an empty
// key, so a second delivery runs the whole accept path.
func TestCollision_ReacceptDoesNotUnsettleAWrittenOffset(t *testing.T) {
	c := newCollisionFixture(t, 4<<20)

	// An untracked article, large enough to flush at once, so it is written.
	if !c.accept(1, "", 0, bytes.Repeat([]byte{'A'}, contiguousRunSize+1)) {
		t.Fatal("precondition: the incumbent was not counted")
	}
	if owner := c.f.w.acceptedAt[0]; !owner.written {
		t.Fatal("precondition: the incumbent was not latched as written")
	}

	// The SAME article delivered again, small enough to sit in the cache
	// rather than write through. Nothing should change about who owns the
	// offset or whether their bytes have landed.
	//
	// A re-accept that writes immediately re-latches the flag in noteWritten
	// and hides the defect, which is why this half has to stay buffered.
	c.accept(1, "", 0, []byte("AAAA"))

	if owner := c.f.w.acceptedAt[0]; !owner.written {
		t.Error("a re-accept by the offset's own owner cleared the written latch, " +
			"unsettling an offset whose bytes are already durable")
	}

	// The consequence: a genuinely different article must still be refused.
	c.accept(2, "", 0, []byte("BBBB"))

	if len(c.rejected) != 1 || c.rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — the offset was unsettled by the "+
			"re-accept, so the arrival displaced an article the barrier has acked",
			c.rejected)
	}
	if got := c.f.w.takeFaulted(); len(got) != 0 {
		t.Errorf("an already-written article was rolled back: %+v — permanently "+
			"failed and acked durable at once", got)
	}
}

// --- Unsettled offsets: the cached incumbent is displaced ------------------

// TestFileWriter_CachedIncumbentIsDisplaced pins the other half. An incumbent
// still sitting in the write cache has made no claim — it was never reported
// Written — so the arrival wins and the incumbent is rolled back, which is
// what the cache's own eviction always did.
func TestFileWriter_CachedIncumbentIsDisplaced(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20))

	first := articleID{msgID: "first", artIdx: 1}
	second := articleID{msgID: "second", artIdx: 2}

	// Small enough that no contiguous run forms, so it stays buffered.
	w.admitAccepted(first.msgID)
	if err := w.Accept(first, 0, append([]byte(nil), bytes.Repeat([]byte{'A'}, 64)...)); err != nil {
		t.Fatalf("accept first: %v", err)
	}
	if len(w.writtenSoFar()) != 0 {
		t.Fatal("precondition: the incumbent was reported Written, so this exercises " +
			"the settled path instead of the displacement one")
	}

	w.admitAccepted(second.msgID)
	if err := w.Accept(second, 0, append([]byte(nil), bytes.Repeat([]byte{'B'}, 64)...)); err != nil {
		t.Fatalf("accept second: %v", err)
	}

	rolled := w.takeFaulted()
	if len(rolled) != 1 || rolled[0].id != first {
		t.Fatalf("rolled back %+v, want just the displaced incumbent %+v", rolled, first)
	}
	if !rolled[0].displaced {
		t.Error("the entry is not marked displaced, so routeFaulted returns it to " +
			"Outstanding and the re-fetched copy displaces its displacer — observed " +
			"as a ping-pong that never settles")
	}
	if got := w.parts(); got != 1 {
		t.Errorf("parts = %d, want 1: two articles admitted, one displaced", got)
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

// TestFileWriter_ReacceptWhileCachedIsNotSelfDisplacement covers the one way
// the SAME article can reach the displaced loop.
//
// handleSuccessArticle's dedup arm returns before acceptArticle when the
// Message-ID is already in seenDone, so a redelivery normally never gets this
// far. An article with no Message-ID cannot be deduped that way — admitAccepted
// declines to record it, because no map can hold an empty key — so a second
// delivery runs the whole accept path again.
//
// Accept's own check correctly says this is not a collision (same owner), but
// wc.buffer evicts the article's previous entry and used to report it as
// displaced regardless of whose it was. The article was then failed as
// displaced BY ITSELF: part given back, faultedArticle appended, a warning
// naming one article twice, and OnArticleRejected resolving it permanently
// failed while its replacement buffer sat in the cache waiting to be written
// and acked. Two terminal dispositions for one article, again.
func TestFileWriter_ReacceptWhileCachedIsNotSelfDisplacement(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20))
	id := articleID{msgID: "", artIdx: 1}

	// Small enough that no contiguous run forms, so it stays buffered.
	w.admitAccepted(id.msgID)
	if err := w.Accept(id, 0, append([]byte(nil), bytes.Repeat([]byte{'A'}, 64)...)); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if !w.wc.buffered(w.key, 0) {
		t.Fatal("precondition: the article is not cache-resident, so the eviction " +
			"path under test is never reached")
	}

	// The same article again, still cache-resident.
	w.admitAccepted(id.msgID)
	if err := w.Accept(id, 0, append([]byte(nil), bytes.Repeat([]byte{'A'}, 64)...)); err != nil {
		t.Fatalf("re-accept: %v", err)
	}

	if rolled := w.takeFaulted(); len(rolled) != 0 {
		t.Errorf("the article was displaced by itself: %+v — routeFaulted resolves it "+
			"permanently failed while its own replacement buffer is still queued to be "+
			"written and acked", rolled)
	}
	if w.postAnomalyReported {
		t.Error("a post anomaly was raised for one article colliding with itself, which " +
			"tells the user their post is malformed when nothing is wrong with it")
	}
}

// TestFileWriter_ThirdArticleAtOneOffsetIsStillDetected pins that detection
// survives its own disposition.
//
// This is the ordering hazard an occupancy-plus-removal design would have hit:
// record the arrival as the new owner, then roll the incumbent back, and the
// rollback deletes the entry just written — so the THIRD article at that
// offset finds it free and goes undetected, which is the original bug
// reintroduced one article later. Identity comparison never removes.
func TestFileWriter_ThirdArticleAtOneOffsetIsStillDetected(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20))

	ids := []articleID{
		{msgID: "a", artIdx: 1}, {msgID: "b", artIdx: 2}, {msgID: "c", artIdx: 3},
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
// hole in failDisplaced's former post-hoc patch-up.
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
// displacement.
func TestFileWriter_DisplacedUntrackedArticleGivesItsPartBack(t *testing.T) {
	w := newTestFileWriter(t, withCacheBytes(1<<20))

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
