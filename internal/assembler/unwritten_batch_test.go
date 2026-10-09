package assembler

import (
	"testing"
)

// TestNoteArticlesUnwritten_IsSilentOnAnEmptySet keeps a no-op from being
// reported as an event.
//
// The callback reaches the queue, and a call with no articles would log a
// warning about a roll-back that did not happen — noise an operator has to
// read past on every successful drain of a file that has nothing buffered.
func TestNoteArticlesUnwritten_IsSilentOnAnEmptySet(t *testing.T) {
	a := newHelperAssembler()
	var calls int
	a.opts.OnArticlesUnwritten = func(string, int, []int32) { calls++ }

	a.noteArticlesUnwritten("job", 0, nil)
	a.noteArticlesUnwritten("job", 0, []int32{})
	if calls != 0 {
		t.Errorf("the callback ran %d times for an empty set", calls)
	}

	a.noteArticlesUnwritten("job", 0, []int32{4})
	if calls != 1 {
		t.Errorf("the callback ran %d times for one article, want 1", calls)
	}
}

// TestFailPermanent_KeepsTheArticleCounted pins the other half of the pair
// fail belongs to.
//
// A rejected article is resolved elsewhere and will never arrive again, so it
// must keep its count toward the file's part total — a file that stopped
// counting it could never reach TotalParts, and the job would sit at 100% with
// nothing outstanding. It must also not appear in the rolled-back set, which
// would clear an Emitted bit the ack is about to resolve.
func TestFailPermanent_KeepsTheArticleCounted(t *testing.T) {
	w := newTestFileWriter(t)
	w.seenDone[1] = struct{}{}

	w.failPermanent(1)

	if _, still := w.seenDone[1]; still {
		t.Error("a permanently refused article is still recorded as done; nothing wrote its bytes")
	}
	if _, failed := w.seenFailed[1]; !failed {
		t.Error("the refused article is not in seenFailed, so a redelivery would be " +
			"counted a second time and overshoot the file's part total")
	}
	if got := w.takeFaulted(); len(got) != 0 {
		t.Errorf("takeFaulted() = %v — a refused article must not be rolled back to "+
			"Outstanding: it is resolved, and re-dispatching it would fetch the same "+
			"unusable article forever", got)
	}

	// An article with no Message-ID is keyed on its ArtIdx like any other —
	// ArtIdx has no value that doubles as "absent", so there is no empty-key
	// case to special-case here.
	w.failPermanent(2)
	if _, failed := w.seenFailed[2]; !failed {
		t.Error("an article with no Message-ID was not recorded in seenFailed under its ArtIdx")
	}
}
