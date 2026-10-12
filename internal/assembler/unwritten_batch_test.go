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
