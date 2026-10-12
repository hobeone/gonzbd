package assembler

import (
	"os"
	"testing"
)

// The part-total transitions, tested against the writer that owns them.
//
// partsWritten is a cached aggregate of seenDone and seenFailed, and every
// defect it produced came from the cache and its source moving at different
// moments. admit and resolve are the only things that move it, so these are
// the tests that pin the arithmetic; the suites that drive processRequest pin
// the routing around them.

// writerState is what one transition leaves behind, read back through the
// writer's own questions where it has them and its maps where it does not.
type writerState struct {
	parts        int
	done, failed bool
}

func stateOf(w *fileWriter, artIdx int32) writerState {
	_, done := w.seenDone[artIdx]
	_, failed := w.seenFailed[artIdx]
	return writerState{parts: w.parts(), done: done, failed: failed}
}

// TestFileWriter_Admit pins the entry: a fresh article is counted and
// recorded, a retry of an article already counted as failed is recorded
// without counting, and a duplicate of an accepted article is refused with
// nothing moved.
//
// The retry case is the one that discriminates: admit decides whether to
// charge a part from what the writer already holds, so a caller cannot reach
// for a counting admission when a non-counting one was owed. That mistake was
// a one-token change at the call site when two admissions existed, with no
// compile-time signal, and it carried the file past TotalParts.
func TestFileWriter_Admit(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *fileWriter)
		want  bool
		state writerState
	}{
		{
			name:  "a fresh article is counted and recorded",
			setup: func(*fileWriter) {},
			want:  true,
			state: writerState{parts: 1, done: true},
		},
		{
			name:  "a retry of an article already counted as failed is recorded without counting",
			setup: func(w *fileWriter) { w.resolve(1, dispositionFetchFailed) },
			want:  true,
			state: writerState{parts: 1, done: true, failed: true},
		},
		{
			name:  "a duplicate of an accepted article is refused with nothing moved",
			setup: func(w *fileWriter) { w.admit(1) },
			want:  false,
			state: writerState{parts: 1, done: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestFileWriter(t)
			tc.setup(w)

			if got := w.admit(1); got != tc.want {
				t.Errorf("admit = %v, want %v", got, tc.want)
			}
			if got := stateOf(w, 1); got != tc.state {
				t.Errorf("state after admit = %+v, want %+v", got, tc.state)
			}
		})
	}
}

// TestFileWriter_Resolve pins the accounting each disposition applies, from
// each state an admitted article can be in when it is resolved.
//
// The table is the spec: one row per (prior state, disposition) pair that a
// production path reaches. The rows that discriminate are the ones where two
// dispositions differ on one column — Rejected and RolledBack both clear
// seenDone, and differ only on the part — and the ones where one disposition
// differs by prior state: RolledBack gives a part back only when the admission
// charged it, so a retry of a failed article keeps the part its failure holds.
func TestFileWriter_Resolve(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *fileWriter)
		d     disposition
		want  writerState
	}{
		{
			name:  "Written keeps the admission",
			setup: func(w *fileWriter) { w.admit(1) },
			d:     dispositionWritten,
			want:  writerState{parts: 1, done: true},
		},
		{
			name:  "Rejected keeps the part and moves the article to seenFailed",
			setup: func(w *fileWriter) { w.admit(1) },
			d:     dispositionRejected,
			want:  writerState{parts: 1, failed: true},
		},
		{
			name: "Rejected after a retry of a failed article keeps the one part the failure holds",
			setup: func(w *fileWriter) {
				w.resolve(1, dispositionFetchFailed)
				w.admit(1)
			},
			d:    dispositionRejected,
			want: writerState{parts: 1, failed: true},
		},
		{
			name:  "RolledBack gives the admission's part back and records no failure",
			setup: func(w *fileWriter) { w.admit(1) },
			d:     dispositionRolledBack,
			want:  writerState{parts: 0},
		},
		{
			name: "RolledBack after a retry of a failed article keeps the part the failure holds",
			setup: func(w *fileWriter) {
				w.resolve(1, dispositionFetchFailed)
				w.admit(1)
			},
			d:    dispositionRolledBack,
			want: writerState{parts: 1, failed: true},
		},
		{
			name:  "RolledBack after Written is the failed-sync rollback",
			setup: func(w *fileWriter) { w.admit(1); w.resolve(1, dispositionWritten) },
			d:     dispositionRolledBack,
			want:  writerState{parts: 0},
		},
		{
			name:  "FetchFailed charges a part for an article the writer never saw",
			setup: func(*fileWriter) {},
			d:     dispositionFetchFailed,
			want:  writerState{parts: 1, failed: true},
		},
		{
			name:  "FetchFailed after Written keeps the written copy and counts nothing",
			setup: func(w *fileWriter) { w.admit(1); w.resolve(1, dispositionWritten) },
			d:     dispositionFetchFailed,
			want:  writerState{parts: 1, done: true, failed: true},
		},
		{
			name:  "a second FetchFailed counts nothing",
			setup: func(w *fileWriter) { w.resolve(1, dispositionFetchFailed) },
			d:     dispositionFetchFailed,
			want:  writerState{parts: 1, failed: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestFileWriter(t)
			// A bystander holding a part, so a transition that touched the
			// wrong article or decremented past its own part would show.
			w.admit(99)
			tc.setup(w)

			w.resolve(1, tc.d)

			want := tc.want
			want.parts++ // the bystander's
			if got := stateOf(w, 1); got != want {
				t.Errorf("state after %s = %+v, want %+v", tc.d, got, want)
			}
		})
	}
}

// TestFileWriter_ResolveRefusesWhatWasNeverAdmitted pins the loud half of the
// seam: a disposition that resolves an admission is refused for an article
// not in seenDone — never admitted, or already resolved out — and the zero
// disposition is refused for every article.
//
// This is the form "an article cannot leave in-flight without a disposition,
// and cannot leave twice" takes where the type system cannot express it. A
// path that forgets to admit, resolves twice, or returns an outcome it never
// filled in fails the first test that walks it rather than undercounting the
// file silently.
func TestFileWriter_ResolveRefusesWhatWasNeverAdmitted(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *fileWriter)
		d     disposition
	}{
		{"Written of an article never admitted", func(*fileWriter) {}, dispositionWritten},
		{"Rejected of an article never admitted", func(*fileWriter) {}, dispositionRejected},
		{"RolledBack of an article never admitted", func(*fileWriter) {}, dispositionRolledBack},
		{"Rejected twice", func(w *fileWriter) { w.admit(1); w.resolve(1, dispositionRejected) }, dispositionRejected},
		{"RolledBack twice", func(w *fileWriter) { w.admit(1); w.resolve(1, dispositionRolledBack) }, dispositionRolledBack},
		{"RolledBack after Rejected", func(w *fileWriter) { w.admit(1); w.resolve(1, dispositionRejected) }, dispositionRolledBack},
		{"the zero disposition", func(w *fileWriter) { w.admit(1) }, disposition(0)},
		{"a disposition past the last", func(w *fileWriter) { w.admit(1) }, dispositionRolledBack + 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestFileWriter(t)
			tc.setup(w)
			before := stateOf(w, 1)

			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("resolve(1, %s) returned; it must refuse an article it cannot settle", tc.d)
				}
				if got := stateOf(w, 1); got != before {
					t.Errorf("state moved from %+v to %+v before the refusal", before, got)
				}
			}()
			w.resolve(1, tc.d)
		})
	}
}

// TestFileWriter_PartsIsTheSizeOfTheUnion pins the identity partsWritten is
// derived from, across every transition in sequence: it equals the size of
// seenDone ∪ seenFailed after each step, which is what makes the cached
// aggregate a cache and not a second opinion.
func TestFileWriter_PartsIsTheSizeOfTheUnion(t *testing.T) {
	w := newTestFileWriter(t)
	check := func(step string) {
		t.Helper()
		union := make(map[int32]struct{}, len(w.seenDone)+len(w.seenFailed))
		for a := range w.seenDone {
			union[a] = struct{}{}
		}
		for a := range w.seenFailed {
			union[a] = struct{}{}
		}
		if got := w.parts(); got != len(union) {
			t.Errorf("after %s: parts() = %d, |seenDone ∪ seenFailed| = %d", step, got, len(union))
		}
	}

	w.admit(1)
	check("admit 1")
	w.resolve(1, dispositionWritten)
	check("1 written")
	w.admit(2)
	check("admit 2")
	w.resolve(2, dispositionRolledBack)
	check("2 rolled back")
	w.admit(3)
	check("admit 3")
	w.resolve(3, dispositionRejected)
	check("3 rejected")
	w.resolve(4, dispositionFetchFailed)
	check("4 fetch-failed")
	w.resolve(4, dispositionFetchFailed)
	check("4 fetch-failed again")
	w.admit(4)
	check("admit 4 as a retry of a failure")
	w.resolve(4, dispositionRolledBack)
	check("4's retry rolled back")
	w.admit(4)
	check("admit 4 again")
	w.resolve(4, dispositionWritten)
	check("4 written")
	w.resolve(1, dispositionFetchFailed)
	check("1 fetch-failed after being written")
	w.resolve(1, dispositionRolledBack)
	check("1 rolled back by a failed sync")
	w.admit(2)
	check("admit 2 again")
	w.resolve(2, dispositionWritten)
	check("2 written")
}

// TestHandleSuccessArticle_RetryOfAFailedArticleIsNotCountedTwice pins the
// CALL SITE, which the method-level tests above cannot: a redelivery of an
// article already resolved permanently failed has its bytes written but is
// not counted a second time.
//
// Counting the retry a second time carries the file PAST TotalParts, so
// finalizeFile fires while other articles are still outstanding — a completion
// event over a file that is not complete.
func TestHandleSuccessArticle_RetryOfAFailedArticleIsNotCountedTwice(t *testing.T) {
	a := newHelperAssembler()
	f := newHelperFile(t, t.TempDir(), "retry-count.dat", 0)

	a.handleFatalArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "m0", FatalErr: os.ErrClosed,
	})
	if f.w.parts() != 1 {
		t.Fatalf("parts() = %d, want 1; the fixture did not reach the state under test",
			f.w.parts())
	}

	a.handleSuccessArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "m0",
		Offset: 0, Data: []byte("abcd"),
	})

	if got := f.w.parts(); got != 1 {
		t.Errorf("parts() = %d after the redelivery, want 1 — one article holding two "+
			"parts carries the file past TotalParts, so finalizeFile fires while other "+
			"articles are still outstanding", got)
	}
	// The bytes are still written, because they are still the file's content.
	if got := f.w.unsynced; len(got) != 1 {
		t.Errorf("unsynced = %v, want one article — the redelivery is not counted, "+
			"but its bytes are the file's content and must still land", got)
	}
}

// TestFileWriter_RolledBackKeepsNoIdentityDependentPath pins that the
// roll-back keys on ArtIdx alone: an article with an empty Message-ID loses
// its part exactly as one with a Message-ID does. Constructible only
// in-package; no production path produces the empty one. It is the input an
// identity-dependent early return would skip.
func TestFileWriter_RolledBackKeepsNoIdentityDependentPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		msgID string
	}{
		{"with a Message-ID", "a@t"},
		{"with none", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestFileWriter(t, withWriteError(os.ErrClosed))
			id := articleID{msgID: tc.msgID, artIdx: 7}

			w.admit(id.artIdx)
			if err := w.accept(id, 0, []byte("xy")); err == nil {
				t.Fatal("accept returned nil after an injected write error")
			}
			w.resolve(id.artIdx, dispositionRolledBack)

			if got := stateOf(w, id.artIdx); got != (writerState{}) {
				t.Errorf("state after the roll-back = %+v, want nothing held: the part "+
					"was not given back, or the article was resolved against itself", got)
			}
		})
	}
}
