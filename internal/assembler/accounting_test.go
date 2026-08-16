package assembler

import "testing"

// The part-total transitions, tested against the writer that owns them.
//
// partsWritten is a cached aggregate of seenDone and seenFailed, and every
// defect it produced came from the cache and its source moving at different
// moments. The methods here are the only things that move it, so these are the
// tests that pin the arithmetic; the suites that drive processRequest pin the
// routing around them.

// TestFileWriter_AdmitCountsAnArticleWithNoMessageID pins the half of
// admitAccepted that the seen-set half hides.
//
// An article with no Message-ID is admitted like any other and counts toward
// TotalParts, but no map can hold it — fail and noteWritten both return early
// on an empty ID. Guarding the COUNT on a non-empty ID, the way the writer's
// other methods guard their map writes, would stop such a file ever reaching
// TotalParts. So the count is deliberately unconditional and the record is not.
func TestFileWriter_AdmitCountsAnArticleWithNoMessageID(t *testing.T) {
	w := newTestFileWriter(t)

	w.admitAccepted("")

	if got := w.parts(); got != 1 {
		t.Errorf("parts() = %d after admitting an article with no Message-ID, want 1 — "+
			"declining to count it puts partsWritten >= TotalParts permanently out of "+
			"reach for the file", got)
	}
	if len(w.seenDone) != 0 {
		t.Errorf("seenDone = %v; an empty Message-ID is not a dedup key and must not be "+
			"stored as one", w.seenDone)
	}
}

// TestFileWriter_AdmitPermanentFailureDedupsTheEmptyMessageID pins the empty
// Message-ID on the FATAL path, which behaves differently from the accept path
// above and is easy to "tidy" into a defect.
//
// admitPermanentFailure writes seenFailed[""] and dedups on it, so the first
// such article is counted and every later one is refused. That is what the call
// site did before the method existed, and it is preserved deliberately.
//
// Two articles are required. One passes whether or not the dedup is there, so a
// single-article test would not discriminate.
func TestFileWriter_AdmitPermanentFailureDedupsTheEmptyMessageID(t *testing.T) {
	w := newTestFileWriter(t)

	if !w.admitPermanentFailure("") {
		t.Fatal("the first fatal article with no Message-ID was not admitted; it has to " +
			"be counted or the file can never reach TotalParts")
	}
	if got := w.parts(); got != 1 {
		t.Fatalf("parts() = %d after the first, want 1", got)
	}

	if w.admitPermanentFailure("") {
		t.Error("a second fatal article with no Message-ID was admitted again; the empty " +
			"key is deduped like any other, and counting it twice carries the file past " +
			"TotalParts")
	}
	if got := w.parts(); got != 1 {
		t.Errorf("parts() = %d after the second, want 1 — the dedup on the seenFailed "+
			"entry is what stops the count running away", got)
	}
}

// TestFileWriter_AdmitRetryOfFailedDoesNotCount pins the arm that records
// without counting.
//
// A redelivery of an article already resolved permanently failed still has its
// bytes written, because they are still the file's content. But the part was
// charged when the article was failed, and charging it again would carry the
// file past TotalParts with one article holding two parts.
func TestFileWriter_AdmitRetryOfFailedDoesNotCount(t *testing.T) {
	w := newTestFileWriter(t)

	if !w.admitPermanentFailure("m1") {
		t.Fatal("precondition: the fatal article should have been admitted")
	}
	before := w.parts()

	w.admitRetryOfFailed("m1")

	if _, recorded := w.seenDone["m1"]; !recorded {
		t.Error("the retry was not recorded in seenDone, so a later copy would be " +
			"written a second time over the same range")
	}
	if got := w.parts(); got != before {
		t.Errorf("parts() = %d after a retry of an already-failed article, want %d — the "+
			"part was charged when it was failed", got, before)
	}
}

// TestFileWriter_FailDisplacedGivesThePartBackAndMarksTheDisposition covers a
// helper that had no direct test reference before this change. It is
// pre-existing debt in a file this change touches rather than anything the
// change introduced, and the gate that surfaced it scans whole files.
//
// failDisplaced is a rollback with a different DISPOSITION from a failed write.
// It gives the article's part back like any rollback, and marks the entry so
// the caller resolves it permanently failed rather than returning it to
// Outstanding — re-fetching it would reproduce the collision that displaced it.
//
// This test pins what the code does today; it does not certify that the pair
// is right. Giving the part back AND resolving the article permanently failed
// is the combination routeAcceptFailure's doc argues against in the mirror
// case, and failPermanent — one function away, reached through the same
// OnArticleRejected route — keeps the part for exactly that reason. Which rule
// should apply here is open: see #379. The change that moved the counter
// preserves the current behaviour rather than settling it, so if #379 lands on
// the other answer, this test's first assertion is the one that flips.
func TestFileWriter_FailDisplacedGivesThePartBackAndMarksTheDisposition(t *testing.T) {
	w := newTestFileWriter(t)
	w.admitAccepted("x1")
	if w.parts() != 1 {
		t.Fatalf("parts() = %d, want 1; the fixture did not admit the article", w.parts())
	}

	w.failDisplaced(articleID{msgID: "x1", artIdx: 1})

	if got := w.parts(); got != 0 {
		t.Errorf("parts() = %d after the displacement, want 0 — this is the CURRENT "+
			"disposition, not an endorsement of it", got)
	}
	if _, still := w.seenDone["x1"]; still {
		t.Error("x1 is still in seenDone after being displaced")
	}
	rolled := w.takeFaulted()
	if len(rolled) != 1 || rolled[0].id.msgID != "x1" {
		t.Fatalf("takeFaulted() = %v, want x1 — an article nobody is told about keeps its "+
			"Emitted bit and is never resolved", rolled)
	}
	if !rolled[0].displaced {
		t.Error("the entry is not marked displaced, so the caller would return it to " +
			"Outstanding; the re-fetched copy then displaces the article that displaced " +
			"it, which was observed as a ping-pong that never settles")
	}
}

// TestFileWriter_GiveBackUntrackedPartReturnsTheCount is the ordinary half of
// the give-back: an article with no Message-ID was admitted, its accept was
// then refused, and the part it was charged has to come back.
//
// It is the only un-admit path the writer cannot derive from its own maps —
// nothing recorded the article, because an empty Message-ID is not a dedup key
// — so the counter is the whole of the state being corrected.
func TestFileWriter_GiveBackUntrackedPartReturnsTheCount(t *testing.T) {
	w := newTestFileWriter(t)
	w.admitAccepted("")
	if w.parts() != 1 {
		t.Fatalf("parts() = %d, want 1; the fixture did not admit the article", w.parts())
	}

	w.giveBackUntrackedPart()

	if got := w.parts(); got != 0 {
		t.Errorf("parts() = %d after the give-back, want 0 — the accept was refused, so "+
			"the file would otherwise sit one part closer to TotalParts with nothing "+
			"behind it", got)
	}
}

// TestFileWriter_GiveBackUntrackedPartIsClampedAtZero pins the degenerate input
// on the one give-back the writer cannot derive from its own maps.
//
// The article has no Message-ID, so nothing records that it ever held a part
// and the method has only the counter to go on. Reaching it at zero would mean
// un-admitting an article that was never admitted; the clamp is what keeps that
// from making the count negative and the file uncompletable.
func TestFileWriter_GiveBackUntrackedPartIsClampedAtZero(t *testing.T) {
	w := newTestFileWriter(t)

	w.giveBackUntrackedPart()

	if got := w.parts(); got != 0 {
		t.Errorf("parts() = %d, want 0 — a give-back with nothing admitted must not "+
			"drive the count below zero", got)
	}
}
