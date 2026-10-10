package assembler

import (
	"bytes"
	"os"
	"slices"
	"testing"
)

// TestOverlap_PartialRangeOverwritesADurableArticle pins #387/#759: two
// articles whose byte ranges overlap without sharing a start offset are
// detected, and the arrival is refused.
//
// A occupies [0, 1000). B occupies [500, 1500). They overlap on [500, 1000);
// A's bytes are on disk before B arrives, so B is the loser.
func TestOverlap_PartialRangeOverwritesADurableArticle(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()

	var rejected []int32
	var unwritten []int32
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}
	a.opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		unwritten = append(unwritten, artIdxs...)
	}

	f := newHelperFile(t, dir, "overlap.dat", 0)
	f.info.TotalParts = 2
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	// A first, fully written before B is submitted.
	a.processRequest(WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "a@example",
		Offset: 0, Data: bytes.Repeat([]byte("A"), 1000),
	}, open, completed)

	// B starts 500 bytes into A's range.
	a.processRequest(WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 1, MessageID: "b@example",
		Offset: 500, Data: bytes.Repeat([]byte("B"), 1000),
	}, open, completed)

	got, err := os.ReadFile(f.info.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	// A's durable range [500, 1000) must still hold A's bytes. If B was allowed
	// to land, this range reads "B" and the run recorded for A — a CRC over
	// [0, 1000) — is asserting a checksum the file no longer satisfies.
	overlap := got[500:1000]
	if !bytes.Equal(overlap, bytes.Repeat([]byte("A"), 500)) {
		t.Errorf("A's durable range [500,1000) holds %q, want all 'A' — B overwrote "+
			"part of an article that was already written, and nothing detected it "+
			"(rejected=%v unwritten=%v)",
			string(overlap[:min(16, len(overlap))]), rejected, unwritten)
	}

	if len(rejected) != 1 || rejected[0] != 1 {
		t.Errorf("OnArticleRejected = %v, want [1] — B claimed [500,1500) overlapping "+
			"A's [0,1000) (file len=%d)", rejected, len(got))
	}
}

// TestOverlap_ContainedOverlapStillCompletesTheFile covers an overlapping
// article that ends at or before the top of the file's accepted ranges, so the
// file does not grow.
//
// A0 [0,100), A1 [100,200), X [150,200). X overlaps A1 without sharing its
// start offset, and is refused.
func TestOverlap_ContainedOverlapStillCompletesTheFile(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()

	var rejected []int32
	var completed int
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}
	a.opts.OnFileComplete = func(_ string, _ int) {
		completed++
	}

	f := newHelperFile(t, dir, "contained.dat", 0)
	f.info.TotalParts = 3
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completedSet := map[fileKey]struct{}{}

	submit := func(idx int32, msg string, off int64, b byte, n int) {
		a.processRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: idx, MessageID: msg,
			Offset: off, Data: bytes.Repeat([]byte{b}, n),
		}, open, completedSet)
	}
	submit(0, "a0@example", 0, 'A', 100)
	submit(1, "a1@example", 100, 'B', 100)
	submit(2, "x@example", 150, 'X', 50) // contained in A1's range

	got, err := os.ReadFile(f.info.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if !bytes.Equal(got[150:200], bytes.Repeat([]byte("B"), 50)) {
		t.Errorf("A1's durable range [150,200) holds %q, want all 'B' — X overwrote "+
			"it undetected (rejected=%v parts=%d completed=%d filelen=%d)",
			string(got[150:200]), rejected, f.w.parts(), completed, len(got))
	}
	if len(rejected) != 1 || rejected[0] != 2 {
		t.Errorf("OnArticleRejected = %v, want [2] — X claimed [150,200) inside A1's "+
			"[100,200) (parts=%d/%d)", rejected, f.w.parts(), f.info.TotalParts)
	}
	if completed != 1 {
		t.Errorf("completed = %d, want 1 — the rejected overlap still counts toward "+
			"TotalParts so the file finishes with its damaged byte count recorded", completed)
	}
}

// TestOverlap_StraddlingArticleIsRefusedAndCountedAsFailed is the #759
// reproduction: A [0, 1000), C [1000, 2000), and B [900, 1900) arriving in the
// order A, C, B.
//
// B's =ypart begin= is off by 100, so its range straddles A's tail [900, 1000)
// and C's head [1000, 1900). Without interval overlap detection, B overwrites
// both neighbours and none of the three articles is charged as failed, so a
// no-par2 job sees contentFailedBytes == 0 (RepairIntact) and ships a splice.
func TestOverlap_StraddlingArticleIsRefusedAndCountedAsFailed(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()

	var rejected []int32
	var unwritten []int32
	var completed int
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}
	a.opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		unwritten = append(unwritten, artIdxs...)
	}
	a.opts.OnFileComplete = func(_ string, _ int) {
		completed++
	}

	f := newHelperFile(t, dir, "straddle.dat", 2000)
	f.info.TotalParts = 3
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completedSet := map[fileKey]struct{}{}

	submit := func(idx int32, msg string, off int64, b byte, n int) {
		a.processRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: idx, MessageID: msg,
			Offset: off, Data: bytes.Repeat([]byte{b}, n),
		}, open, completedSet)
	}

	// Arrive in the order A [0, 1000), C [1000, 2000), B [900, 1900).
	submit(0, "a@example", 0, 'A', 1000)
	submit(2, "c@example", 1000, 'C', 1000)
	submit(1, "b@example", 900, 'B', 1000)

	drained, err := f.w.Drain()
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, d := range drained {
		if d.ArtIdx == 1 {
			t.Errorf("straddling article B (artIdx 1) was reported Written in Drain: %+v", d)
		}
	}

	got, err := os.ReadFile(f.info.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) < 2000 {
		t.Fatalf("file length = %d, want at least 2000", len(got))
	}
	if !bytes.Equal(got[:1000], bytes.Repeat([]byte("A"), 1000)) {
		t.Errorf("A's range [0,1000) was corrupted by straddling article B")
	}
	if !bytes.Equal(got[1000:2000], bytes.Repeat([]byte("C"), 1000)) {
		t.Errorf("C's range [1000,2000) was corrupted by straddling article B")
	}
	if len(rejected) != 1 || rejected[0] != 1 {
		t.Errorf("OnArticleRejected = %v, want [1] (B refused and counted as failed)", rejected)
	}
	if len(unwritten) != 0 {
		t.Errorf("OnArticlesUnwritten = %v, want empty", unwritten)
	}
	if f.w.parts() != 3 || completed != 1 {
		t.Errorf("parts = %d, completed = %d, want parts=3 completed=1", f.w.parts(), completed)
	}
}

// TestOverlap_BadArticleArrivingFirstRefusesItsNeighbours pins the accepted
// arrival-order limitation of #759: when the misplaced article B [900, 1900)
// arrives before A [0, 1000) and C [1000, 2000), A and C both intersect B's
// owned range and are refused so no splice is written.
func TestOverlap_BadArticleArrivingFirstRefusesItsNeighbours(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()

	var rejected []int32
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}

	f := newHelperFile(t, dir, "bad-first.dat", 2000)
	f.info.TotalParts = 3
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completedSet := map[fileKey]struct{}{}

	submit := func(idx int32, msg string, off int64, b byte, n int) {
		a.processRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: idx, MessageID: msg,
			Offset: off, Data: bytes.Repeat([]byte{b}, n),
		}, open, completedSet)
	}

	// B arrives first, followed by A and C.
	submit(1, "b@example", 900, 'B', 1000)
	submit(0, "a@example", 0, 'A', 1000)
	submit(2, "c@example", 1000, 'C', 1000)

	if len(rejected) != 2 || rejected[0] != 0 || rejected[1] != 2 {
		t.Errorf("OnArticleRejected = %v, want [0 2] (both neighbours refused)", rejected)
	}
	if f.w.parts() != 3 {
		t.Errorf("parts = %d, want 3", f.w.parts())
	}
}

// TestOverlap_SameOffsetDifferentLengthIsRefused pins that an arriving article
// sharing start offset 0 with a written incumbent [0, 1000) is refused at any
// non-zero length ([0, 500) or [0, 1500)), so the incumbent is neither
// partially overwritten nor extended over the neighbour at [1000, 2000). A
// zero-length arrival occupies no bytes, intersects nothing, and is accepted
// without touching A.
func TestOverlap_SameOffsetDifferentLengthIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name         string
		arrivalLen   int
		wantRejected []int32
	}{
		{"zero_0", 0, nil},
		{"shorter_500", 500, []int32{1}},
		{"longer_1500", 1500, []int32{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arrivalLen := tc.arrivalLen
			dir := t.TempDir()
			a := newHelperAssembler()

			var rejected []int32
			a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
				rejected = append(rejected, artIdx)
			}

			f := newHelperFile(t, dir, "same-off-diff-len.dat", 2000)
			f.info.TotalParts = 3
			key := fileKey{jobID: "job", fileIdx: 0}
			open := map[fileKey]*openFile{key: f}
			completedSet := map[fileKey]struct{}{}

			submit := func(idx int32, msg string, off int64, b byte, n int) {
				a.processRequest(WriteRequest{
					JobID: "job", FileIdx: 0, ArtIdx: idx, MessageID: msg,
					Offset: off, Data: bytes.Repeat([]byte{b}, n),
				}, open, completedSet)
			}

			submit(0, "a@example", 0, 'A', 1000)
			// B claims the same start offset 0 with a different length.
			submit(1, "b@example", 0, 'B', arrivalLen)
			// C [1000, 2000) completes the file.
			submit(2, "c@example", 1000, 'C', 1000)

			if !slices.Equal(rejected, tc.wantRejected) {
				t.Errorf("OnArticleRejected = %v, want %v (B at A's start offset, len %d)",
					rejected, tc.wantRejected, arrivalLen)
			}
			got, err := os.ReadFile(f.w.path)
			if err != nil {
				t.Fatalf("read file: %v", err)
			}
			if len(got) != 2000 {
				t.Fatalf("file len = %d, want 2000", len(got))
			}
			if !bytes.Equal(got[:1000], bytes.Repeat([]byte{'A'}, 1000)) {
				t.Errorf("A's range [0,1000) was overwritten or truncated by B (len=%d)", arrivalLen)
			}
			if !bytes.Equal(got[1000:2000], bytes.Repeat([]byte{'C'}, 1000)) {
				t.Errorf("C's range [1000,2000) was overwritten by B (len=%d)", arrivalLen)
			}
		})
	}
}

// TestOverlap_ZeroLengthArticleDoesNotBlockCoveringNeighbour pins that a
// zero-length article accepted at an interior offset [500, 500) does not
// block a subsequent legitimate article [0, 1000) covering offset 500, nor
// break the owned set's ordering for later overlap checks.
func TestOverlap_ZeroLengthArticleDoesNotBlockCoveringNeighbour(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()

	var rejected []int32
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}

	f := newHelperFile(t, dir, "zero-interior.dat", 2000)
	f.info.TotalParts = 4
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completedSet := map[fileKey]struct{}{}

	submit := func(idx int32, msg string, off int64, data []byte) {
		a.processRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: idx, MessageID: msg,
			Offset: off, Data: data,
		}, open, completedSet)
	}

	// Zero-length article Z arrives first at interior offset 500.
	submit(0, "z@example", 500, nil)
	// Legitimate article A [0, 1000) covers offset 500 and must be accepted.
	submit(1, "a@example", 0, bytes.Repeat([]byte{'A'}, 1000))
	// Legitimate article C [1000, 2000) abuts A and must be accepted.
	submit(2, "c@example", 1000, bytes.Repeat([]byte{'C'}, 1000))
	// Overlapping article S [600, 900) inside A past offset 500 must still be refused.
	submit(3, "s@example", 600, bytes.Repeat([]byte{'S'}, 300))

	if len(rejected) != 1 || rejected[0] != 3 {
		t.Fatalf("OnArticleRejected = %v, want [3] (overlapping S refused)", rejected)
	}
	got, err := os.ReadFile(f.w.path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(got[:1000], bytes.Repeat([]byte{'A'}, 1000)) {
		t.Error("A's range [0,1000) was not written intact")
	}
	if !bytes.Equal(got[1000:2000], bytes.Repeat([]byte{'C'}, 1000)) {
		t.Error("C's range [1000,2000) was not written intact")
	}
}

// TestOverlap_ArrivalStartingBeforeTheIncumbentIsRefused pins that
// acceptArticle probes the arrival's whole range, not its first byte. In both
// cases the arrival starts BEFORE the incumbent, so a probe of its first byte
// alone finds nothing, the arrival is written over the incumbent, and claim
// panics on the intersecting entry.
func TestOverlap_ArrivalStartingBeforeTheIncumbentIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name              string
		incOff, arrOff    int64
		incLen, arrLen    int
		incByte, arrivals byte
	}{
		{"left_straddle", 1000, 900, 1000, 200, 'C', 'B'},
		{"enclosing", 3000, 2500, 100, 1000, 'D', 'E'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newHelperAssembler()
			var rejected []int32
			a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
				rejected = append(rejected, artIdx)
			}
			f := newHelperFile(t, t.TempDir(), "before.dat", 4000)
			f.info.TotalParts = 2
			open := map[fileKey]*openFile{{jobID: "job", fileIdx: 0}: f}
			completedSet := map[fileKey]struct{}{}
			submit := func(idx int32, off int64, b byte, n int) {
				t.Helper()
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("article %d at [%d,%d) panicked the worker: %v", idx, off, off+int64(n), p)
					}
				}()
				a.processRequest(WriteRequest{
					JobID: "job", FileIdx: 0, ArtIdx: idx, MessageID: string('a'+idx) + "@example",
					Offset: off, Data: bytes.Repeat([]byte{b}, n),
				}, open, completedSet)
			}

			submit(0, tc.incOff, tc.incByte, tc.incLen)
			submit(1, tc.arrOff, tc.arrivals, tc.arrLen)

			if !slices.Equal(rejected, []int32{1}) {
				t.Errorf("OnArticleRejected = %v, want [1] — the arrival [%d,%d) intersects the "+
					"incumbent [%d,%d)", rejected, tc.arrOff, tc.arrOff+int64(tc.arrLen),
					tc.incOff, tc.incOff+int64(tc.incLen))
			}
			got, err := os.ReadFile(f.info.Path)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			inc := got[tc.incOff : tc.incOff+int64(tc.incLen)]
			if !bytes.Equal(inc, bytes.Repeat([]byte{tc.incByte}, tc.incLen)) {
				t.Errorf("the incumbent's range was overwritten by the arrival")
			}
		})
	}
}
