package assembler

import (
	"os"
	"testing"
)

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

// TestFileWriter_FailDisplacedKeepsThePartAndMarksTheDisposition pins the
// disposition #386 settled: a displaced article is RESOLVED, not rolled back.
//
// The question this leaves open was which of two rules applies to an article
// that loses its offset. Giving the part back AND resolving it permanently
// failed is the combination routeAcceptFailure's doc argues against in the
// mirror case, and it is unsatisfiable: TotalParts counts both colliding
// segments, so a file that stops counting the loser waits for a part that
// nothing can now supply.
//
// So the part stays. The seen-set half is what makes a redelivery cheap —
// handleSuccessArticle and handleLateDuplicate both test seenDone before
// seenFailed, and membership in either is enough to refuse the copy without
// re-writing it.
func TestFileWriter_FailDisplacedKeepsThePartAndMarksTheDisposition(t *testing.T) {
	w := newTestFileWriter(t)
	w.admitAccepted("x1")
	if w.parts() != 1 {
		t.Fatalf("parts() = %d, want 1; the fixture did not admit the article", w.parts())
	}

	w.failDisplaced(articleID{msgID: "x1", artIdx: 1}, 0, articleID{msgID: "x2", artIdx: 2})

	if got := w.parts(); got != 1 {
		t.Errorf("parts() = %d after the displacement, want 1 — the article is resolved "+
			"permanently failed, and a file that stopped counting it could never reach "+
			"TotalParts", got)
	}
	if _, failed := w.seenFailed["x1"]; !failed {
		t.Error("x1 is not in seenFailed after being displaced, so a redelivery is not " +
			"recognised and gets reported permanently failed a second time")
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

// TestFileWriter_FailKeepsThePartOfAnAlreadyFailedArticle pins the !wasFailed
// half of fail's give-back, which nothing else in the package observes.
//
// The two halves of the condition guard opposite mistakes and only one of them
// was pinned. An article counted as WRITTEN must lose its part when a write
// rolls it back; an article counted as permanently FAILED must NOT, because
// admitPermanentFailure charged that part against the seenFailed record and a
// redelivery writes its bytes without charging a second one
// (admitRetryOfFailed). Giving a part back that this path never took leaves the
// file permanently one short: partsWritten >= TotalParts becomes unreachable,
// OnFileComplete never fires, MarkFileComplete never runs, and the job sits at
// 100% with nothing outstanding across restarts.
//
// Confirmed discriminating: neutering `!wasFailed` in fail leaves every other
// test in the package green.
func TestFileWriter_FailKeepsThePartOfAnAlreadyFailedArticle(t *testing.T) {
	w := newTestFileWriter(t)
	if !w.admitPermanentFailure("m1") {
		t.Fatal("precondition: the fatal article should have been admitted")
	}
	// The redelivery: its bytes are the file's content, but the part was
	// charged when the article was failed, so nothing is counted here.
	w.admitRetryOfFailed("m1")
	if w.parts() != 1 {
		t.Fatalf("parts() = %d, want 1; the fixture did not reach the state under test",
			w.parts())
	}

	// The write of that redelivery fails.
	w.fail(articleID{msgID: "m1", artIdx: 1})

	if got := w.parts(); got != 1 {
		t.Errorf("parts() = %d after rolling back a retry of an already-failed "+
			"article, want 1 — the part belongs to the permanent failure, and giving "+
			"back one this path never took leaves the file one short of TotalParts "+
			"forever", got)
	}
	if _, still := w.seenDone["m1"]; still {
		t.Error("m1 is still in seenDone after the roll-back, so a redelivery would " +
			"be read as a duplicate and its bytes never written")
	}
}

// TestFileWriter_RollbackPart covers the give-back both dispositions share.
//
// rollbackPart is now fail's alone. It used to be shared with failDisplaced,
// on the premise that the two dispositions differed in what they RECORDED but
// not in how they counted; #386 retired that premise, since a displaced
// article keeps its part and a rolled-back one gives it up.
//
// It is tested directly because the branching lives here rather than at either
// call site, so a change to fail's accounting cannot pass unnoticed.
//
// There is no untracked case any more. fail returns early on an empty
// Message-ID before reaching this, so the branch that handled one was removed
// with its last caller rather than left reachable only from this test.
func TestFileWriter_RollbackPart(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(w *FileWriter)
		id        articleID
		wantParts int
	}{
		{
			name:      "an accepted article gives its part back",
			setup:     func(w *FileWriter) { w.admitAccepted("m1") },
			id:        articleID{msgID: "m1", artIdx: 1},
			wantParts: 0,
		},
		{
			name: "an already-failed article keeps its part",
			setup: func(w *FileWriter) {
				w.admitPermanentFailure("m1")
				w.admitRetryOfFailed("m1")
			},
			id:        articleID{msgID: "m1", artIdx: 1},
			wantParts: 1,
		},
		{
			name:      "an article that never held a part takes none away",
			setup:     func(w *FileWriter) { w.admitAccepted("other") },
			id:        articleID{msgID: "m1", artIdx: 1},
			wantParts: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestFileWriter(t)
			tc.setup(w)

			w.rollbackPart(tc.id)

			if got := w.parts(); got != tc.wantParts {
				t.Errorf("parts() = %d, want %d", got, tc.wantParts)
			}
			if tc.id.msgID != "" {
				if _, still := w.seenDone[tc.id.msgID]; still {
					t.Errorf("%s is still in seenDone, so a redelivery would be read "+
						"as a duplicate and its bytes never written", tc.id.msgID)
				}
			}
			// The give-back is the whole of it: rollbackPart records no
			// disposition, so neither caller can inherit one it did not choose.
			if len(w.faulted) != 0 {
				t.Errorf("rollbackPart appended %d faulted articles, want 0 — the "+
					"disposition belongs to the caller", len(w.faulted))
			}
		})
	}
}

// TestHandleSuccessArticle_RetryOfAFailedArticleIsNotCountedTwice pins the
// CALL SITE, which the method-level tests above cannot.
//
// admitAccepted and admitRetryOfFailed differ only in whether they count, so
// handleSuccessArticle's seenFailed arm reaching for the wrong one is a
// one-token mistake with no compile-time signal. Confirmed discriminating:
// swapping the call for admitAccepted left the whole package green before this
// test existed.
//
// Counting the retry a second time carries the file PAST TotalParts, so
// finalizeFile fires while other articles are still outstanding — a completion
// event over a file that is not complete.
func TestHandleSuccessArticle_RetryOfAFailedArticleIsNotCountedTwice(t *testing.T) {
	a := newHelperAssembler()
	f := newHelperFile(t, t.TempDir(), "retry-count.dat", 0)

	if !a.handleFatalArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "m0", FatalErr: os.ErrClosed,
	}) {
		t.Fatal("precondition: the permanently failed article was not counted")
	}
	if f.w.parts() != 1 {
		t.Fatalf("parts() = %d, want 1; the fixture did not reach the state under test",
			f.w.parts())
	}

	counted := a.handleSuccessArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "m0",
		Offset: 0, Data: []byte("abcd"),
	})

	if counted {
		t.Error("the redelivery was reported as a new part; its part was charged when " +
			"the article was failed")
	}
	if got := f.w.parts(); got != 1 {
		t.Errorf("parts() = %d after the redelivery, want 1 — one article holding two "+
			"parts carries the file past TotalParts, so finalizeFile fires while other "+
			"articles are still outstanding", got)
	}
	// The bytes are still written, because they are still the file's content.
	if got := f.w.writtenSoFar(); len(got) != 1 {
		t.Errorf("writtenSoFar = %v, want one article — the redelivery is not counted, "+
			"but its bytes are the file's content and must still land", got)
	}
}

// TestFaultedIndices_ListsEveryArticleInTheSet pins the tripwire logs' one
// derivation. The cancel arm DROPS the set it reports, so this slice is the
// only record left of which articles were stranded; a helper that dropped or
// reordered an entry would make that record quietly wrong.
func TestFaultedIndices_ListsEveryArticleInTheSet(t *testing.T) {
	got := faultedIndices([]faultedArticle{
		{id: articleID{msgID: "n1", artIdx: 7}},
		{id: articleID{msgID: "d2", artIdx: 2}, displaced: true},
	})
	if len(got) != 2 || got[0] != 7 || got[1] != 2 {
		t.Errorf("faultedIndices = %v, want [7 2] — every article in the set, in order, "+
			"whatever its disposition", got)
	}
	if got := faultedIndices(nil); len(got) != 0 {
		t.Errorf("faultedIndices(nil) = %v, want empty", got)
	}
}
