package durability

import (
	"bytes"
	"hash/crc32"
	"testing"

	"github.com/hobeone/gonzbd/internal/crc32util"
)

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
