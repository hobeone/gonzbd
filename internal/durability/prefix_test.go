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

	// A ZERO-LENGTH fact below the run. It stops the walk, but it overwrote
	// nothing: the two ranges share no byte, so there is no overlap to report.
	// Classifying it would name two articles over a range no article claims.
	empty := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 100, CRC32: crcA},
		{ArtIdx: 1, Offset: 100, Length: 100, CRC32: crcA},
		{ArtIdx: 2, Offset: 150, Length: 0},
	}
	if _, _, ok := verifiedPrefix(empty, all).overlap(); ok {
		t.Error("overlap() reported true for a zero-length fact below the run — it " +
			"describes no bytes, so nothing of the sibling was overwritten")
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
