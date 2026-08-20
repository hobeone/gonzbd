package durability

import (
	"bytes"
	"hash/crc32"
	"testing"

	"github.com/hobeone/gonzbd/internal/crc32util"
)

// TestVerifiedPrefix_DistinguishesAnOverlapFromAHole pins the classification
// the walk already had the information for and used to discard.
//
// The walk stops at the first fact that does not abut the run, and its own
// comment names both causes: a hole, or an overlap. Only the second means two
// durable articles claim the same bytes, and only the second may be reported —
// telling a user their post is malformed because a file is merely incomplete
// would fire on every running download.
func TestVerifiedPrefix_DistinguishesAnOverlapFromAHole(t *testing.T) {
	crcA := uint32(0x11111111)
	all := func(int) bool { return true }

	// A0 [0,100), A1 [100,200), X [150,200) — X starts BELOW the run.
	overlapping := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
		{ArtIdx: 1, Offset: 100, Length: 100, CRC32: crcA},
		{ArtIdx: 2, Offset: 150, Length: 50, CRC32: crcA},
	}
	victim, arrival, ok := verifiedPrefix(overlapping, all).overlap()
	if !ok || victim.ArtIdx != 1 || arrival.ArtIdx != 2 {
		t.Errorf("overlap() = (#%d, #%d, %v), want (#1, #2, true) — X starts at 150 "+
			"inside a run that already reached 200, so two durable articles claim "+
			"those bytes, and the sibling it landed inside is A1 rather than A0",
			victim.ArtIdx, arrival.ArtIdx, ok)
	}

	// A0 [0,100), then a fact at 200 — a HOLE, not an overlap. Reporting this
	// would tell the user their post is malformed when the file is merely
	// incomplete, which is the ordinary state of a running download.
	gapped := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
		{ArtIdx: 2, Offset: 200, Length: 100, CRC32: crcA},
	}
	if _, _, ok := verifiedPrefix(gapped, all).overlap(); ok {
		t.Error("overlap() reported true for a hole — a file waiting on an article " +
			"would be reported as a malformed post on every checkpoint")
	}

	// A ZERO-LENGTH fact below the run overwrote nothing: the two ranges share
	// no byte, so there is no overlap to report.
	empty := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
		{ArtIdx: 1, Offset: 100, Length: 100, CRC32: crcA},
		{ArtIdx: 2, Offset: 150, Length: 0},
	}
	if _, _, ok := verifiedPrefix(empty, all).overlap(); ok {
		t.Error("overlap() reported true for a zero-length fact below the run — it " +
			"describes no bytes, so nothing of the sibling was overwritten")
	}

	// ...and it must not stop the walk either, or the REAL overlap after it is
	// never examined. This is the case that makes skipping the empty fact
	// mandatory rather than tidy: treating it as a terminal stop trades a false
	// warning for a missing one, and a silently malformed file is the worse
	// outcome. X at 75 lands inside A0's [0,100) and must still be found.
	emptyThenReal := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
		{ArtIdx: 1, Offset: 50, Length: 0},
		{ArtIdx: 2, Offset: 75, Length: 100, CRC32: crcA},
	}
	victim, arrival, ok = verifiedPrefix(emptyThenReal, all).overlap()
	if !ok || victim.ArtIdx != 0 || arrival.ArtIdx != 2 {
		t.Errorf("overlap() = (#%d, #%d, %v), want (#0, #2, true) — an empty fact "+
			"before the real overlap must not end the walk, and the article X landed "+
			"inside is the last NON-EMPTY one",
			victim.ArtIdx, arrival.ArtIdx, ok)
	}

	// An UNVERIFIED fact stops the walk before the offset is ever compared, so
	// it must not be classified either way. This is what keeps the assembler's
	// own exact-offset collisions from being reported twice: its loser is
	// resolved permanently failed and never earns a durable bit.
	//
	// The unverified fact must ALSO be the overlapping one. With verified =
	// {true, false, ...} the walk stops at A1, which ABUTS, so the offset test
	// cannot fire there under either ordering and the mutation below would be
	// invisible — a green that reads as evidence and is nothing of the kind.
	twoOfThree := func(i int) bool { return i < 2 }
	if _, _, ok := verifiedPrefix(overlapping, twoOfThree).overlap(); ok {
		t.Error("overlap() reported true when the walk stopped on an unverified " +
			"fact; the assembler's own collisions would be double-reported")
	}
}

// TestOverlapAnywhere_FindsAnOverlapAboveAHole is the case the prefix walk
// structurally cannot see, and the reason a second classifier exists.
//
// A0 [0,100), nothing covering [100,200), then A2 [200,300) and A3 [250,300).
// A2 and A3 both landed and both claim [250,300). The walk halts at the hole —
// it must, because a contiguous prefix is what it computes — so it never
// examines either. For a file being finalized that hole is permanent, and no
// later checkpoint or finalize ever revisits it, so the walk's silence would be
// final.
//
// The test asserts BOTH: that the walk misses it and that the scan finds it.
// Asserting only the second would pass just as well if the walk had found it
// too, and would then say nothing about why this function is needed.
func TestOverlapAnywhere_FindsAnOverlapAboveAHole(t *testing.T) {
	all := func(int) bool { return true }
	facts := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100},
		{ArtIdx: 2, Offset: 200, Length: 100},
		{ArtIdx: 3, Offset: 250, Length: 50},
	}

	if _, _, ok := verifiedPrefix(facts, all).overlap(); ok {
		t.Error("the prefix walk reported an overlap above a hole — if it can see " +
			"this, overlapAnywhere is redundant and should be deleted rather than kept")
	}

	victim, arrival, ok := overlapAnywhere(facts, all)
	if !ok || victim.ArtIdx != 2 || arrival.ArtIdx != 3 {
		t.Errorf("overlapAnywhere = (#%d, #%d, %v), want (#2, #3, true) — a file "+
			"finalized with a permanent gap would never report the overlap above it",
			victim.ArtIdx, arrival.ArtIdx, ok)
	}
}

// TestOverlapAnywhere_Table covers what the sweep must NOT call an overlap, and
// the containment case that decides how it tracks its predecessor.
func TestOverlapAnywhere_Table(t *testing.T) {
	all := func(int) bool { return true }
	tests := []struct {
		name          string
		facts         []ArticleFact
		verified      func(int) bool
		wantOK        bool
		victim, first int32
	}{
		{
			name:     "a tiling file has no overlap",
			facts:    []ArticleFact{{ArtIdx: 0, Offset: 0, Length: 100}, {ArtIdx: 1, Offset: 100, Length: 100}},
			verified: all,
		},
		{
			name:     "a hole alone is not an overlap",
			facts:    []ArticleFact{{ArtIdx: 0, Offset: 0, Length: 100}, {ArtIdx: 1, Offset: 200, Length: 100}},
			verified: all,
		},
		{
			// The skip must be a skip and not a stop, and only an UNVERIFIED
			// fact shows that. An offset gap does not exercise it — this sweep
			// has no gap test to stop on — so a fixture built from a hole alone
			// passes just as well when the skip is turned into a break.
			name: "an unverified fact does not end the sweep",
			facts: []ArticleFact{
				{ArtIdx: 0, Offset: 0, Length: 100},
				{ArtIdx: 1, Offset: 100, Length: 100},
				{ArtIdx: 2, Offset: 200, Length: 100},
				{ArtIdx: 3, Offset: 250, Length: 50},
			},
			verified: func(i int) bool { return i != 1 },
			wantOK:   true, victim: 2, first: 3,
		},
		{
			// The same, for the other skip.
			name: "a zero-length fact does not end the sweep",
			facts: []ArticleFact{
				{ArtIdx: 0, Offset: 0, Length: 100},
				{ArtIdx: 1, Offset: 120, Length: 0},
				{ArtIdx: 2, Offset: 200, Length: 100},
				{ArtIdx: 3, Offset: 250, Length: 50},
			},
			verified: all, wantOK: true, victim: 2, first: 3,
		},
		{
			// The arrival is not durable, so nothing of it is on disk to have
			// overwritten the sibling it nominally covers.
			name:     "an unverified arrival is skipped, not reported",
			facts:    []ArticleFact{{ArtIdx: 0, Offset: 0, Length: 200}, {ArtIdx: 1, Offset: 50, Length: 50}},
			verified: func(i int) bool { return i == 0 },
		},
		{
			name:     "a zero-length fact inside a range is not an overlap",
			facts:    []ArticleFact{{ArtIdx: 0, Offset: 0, Length: 200}, {ArtIdx: 1, Offset: 50, Length: 0}},
			verified: all,
		},
		{
			// A wholly contained article is an overlap, and the FIRST one wins.
			// #1 and #2 both sit inside #0; the sweep returns at #1 and never
			// examines #2, which is the same "damage report, not inventory"
			// rule the prefix walk follows.
			name: "a contained article is reported, and the lowest one wins",
			facts: []ArticleFact{
				{ArtIdx: 0, Offset: 0, Length: 200},
				{ArtIdx: 1, Offset: 50, Length: 10},
				{ArtIdx: 2, Offset: 70, Length: 10},
			},
			verified: all, wantOK: true, victim: 0, first: 1,
		},
		{
			// The advance path, which the cases above never exercise past the
			// first pair: three tiling articles must walk to the end and report
			// nothing, so a broken advance shows up as a spurious finding.
			name: "three tiling articles advance cleanly to the end",
			facts: []ArticleFact{
				{ArtIdx: 0, Offset: 0, Length: 100},
				{ArtIdx: 1, Offset: 100, Length: 100},
				{ArtIdx: 2, Offset: 200, Length: 100},
			},
			verified: all,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, a, ok := overlapAnywhere(tc.facts, tc.verified)
			if ok != tc.wantOK {
				t.Fatalf("overlapAnywhere ok = %v, want %v (got #%d, #%d)", ok, tc.wantOK, v.ArtIdx, a.ArtIdx)
			}
			if ok && (v.ArtIdx != tc.victim || a.ArtIdx != tc.first) {
				t.Errorf("overlapAnywhere = (#%d, #%d), want (#%d, #%d)",
					v.ArtIdx, a.ArtIdx, tc.victim, tc.first)
			}
		})
	}
}

// TestVerifiedPrefix_TheZeroWalkReportsNoOverlap pins the zero value, which
// buildExtent returns on four error paths. A sentinel rather than an explicit
// bool would make the zero walk claim an overlap between article 0 and itself.
func TestVerifiedPrefix_TheZeroWalkReportsNoOverlap(t *testing.T) {
	if v, a, ok := (prefixWalk{}).overlap(); ok {
		t.Errorf("the zero prefixWalk reported an overlap between #%d and #%d",
			v.ArtIdx, a.ArtIdx)
	}
}

// TestVerifiedPrefix_Table exercises the walk directly, over the fact/verified
// shapes that are awkward to reach through Resume: an empty fact list, a
// verified run that stops because the next fact was not proven, and one that
// stops because the next fact does not abut it.
func TestVerifiedPrefix_Table(t *testing.T) {
	a := bytes.Repeat([]byte{0x01}, 100)
	b := bytes.Repeat([]byte{0x02}, 100)
	crcA, crcB := crc32.ChecksumIEEE(a), crc32.ChecksumIEEE(b)
	abut := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
		{ArtIdx: 1, Offset: 100, Length: 100, CRC32: crcB},
	}
	gapped := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
		{ArtIdx: 2, Offset: 200, Length: 100, CRC32: crcB},
	}

	tests := []struct {
		name      string
		facts     []ArticleFact
		verified  []bool
		size      int64
		wantTo    int64
		wantCRC   uint32
		wantWhole bool
	}{
		{
			// "Vacuously whole" was the reading this case used to encode, and
			// it is the one thing the flag must not mean. HasPrefixCRC says
			// "this is a verified whole-file CRC"; a file with no facts has
			// been verified against nothing, and the 0 it would report — a
			// correct CRC32 of zero bytes — is indistinguishable from a real
			// answer to the QuickCheck comparison downstream.
			name: "no facts and no bytes verifies nothing, so claims nothing",
			size: 0,
		},
		{
			name: "no facts but bytes on disk is not whole",
			size: 100,
		},
		{
			name:  "both verified and abutting reaches the end",
			facts: abut, verified: []bool{true, true}, size: 200,
			wantTo: 200, wantCRC: crc32util.Combine(crcA, crcB, 100), wantWhole: true,
		},
		{
			name:  "an unverified second fact stops the run short",
			facts: abut, verified: []bool{true, false}, size: 200,
			wantTo: 100, wantCRC: crcA,
		},
		{
			name:  "a verified fact that does not abut stops the run",
			facts: gapped, verified: []bool{true, true}, size: 300,
			wantTo: 100, wantCRC: crcA,
		},
		{
			name:  "an unverified first fact yields nothing",
			facts: abut, verified: []bool{false, true}, size: 200,
		},
		{
			name:  "reaching the end with a fact left over is not whole",
			facts: abut, verified: []bool{true, false}, size: 100,
			wantTo: 100, wantCRC: crcA,
		},
		{
			// #387's shape, and the reason the consumed clause exists. The case
			// above reaches the same clause through an UNVERIFIED fact; this one
			// reaches it through a verified fact that overlaps a sibling, which
			// is the input that actually occurs. A0 and A1 tile [0,200), so the
			// prefix reaches the file's end with X left over — and a CRC over
			// [0,200) describes the bytes that SHOULD be there, which is what
			// par2 compares against. Publishing it makes QuickCheck report clean
			// and skips the repair that would have fixed the file.
			name: "an overlapping fact left over is not whole",
			facts: []ArticleFact{
				{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
				{ArtIdx: 1, Offset: 100, Length: 100, CRC32: crcB},
				{ArtIdx: 2, Offset: 150, Length: 50, CRC32: crcB},
			},
			verified: []bool{true, true, true}, size: 200,
			wantTo: 200, wantCRC: crc32util.Combine(crcA, crcB, 100),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := verifiedPrefix(tc.facts, func(i int) bool { return tc.verified[i] })
			to, crc, whole := w.VerifiedTo, w.PrefixCRC, w.wholeFile(tc.size)
			if to != tc.wantTo || crc != tc.wantCRC || whole != tc.wantWhole {
				t.Errorf("verifiedPrefix = (%d, %#x, %v), want (%d, %#x, %v)",
					to, crc, whole, tc.wantTo, tc.wantCRC, tc.wantWhole)
			}
		})
	}
}
