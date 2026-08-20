package durability

import (
	"bytes"
	"hash/crc32"
	"testing"

	"github.com/hobeone/gonzbd/internal/crc32util"
)

// TestVerifiedPrefix_AnUnconsumedFactWithholdsTheWholeFileClaim is the pin for
// the defect this function was extracted to fix.
//
// A0 [0,100), A1 [100,200), X [150,200). X overlaps A1 without sharing a start
// offset, so the walk tiles [0,200) from A0+A1 and breaks at X. The prefix
// reaches the file's end while a fact remains unconsumed.
//
// Reporting wholeFile here publishes a CRC combined from the facts — which are
// built from each article's own decoded bytes before the write, so they describe
// the file that SHOULD have been written. That is also what par2 describes, so
// the wrong value matches par2, QuickCheck reports clean, repair is skipped and
// the recovery volumes are never fetched. Barrier used to do exactly this; the
// equivalent walk on the resume path did not.
func TestVerifiedPrefix_AnUnconsumedFactWithholdsTheWholeFileClaim(t *testing.T) {
	facts := []ArticleFact{
		{Offset: 0, Length: 100, CRC32: 0x11111111},
		{Offset: 100, Length: 100, CRC32: 0x22222222},
		{Offset: 150, Length: 50, CRC32: 0x33333333},
	}
	all := func(int) bool { return true }

	to, _, whole := verifiedPrefix(facts, all, 200)
	if to != 200 {
		t.Errorf("verifiedTo = %d, want 200 — A0 and A1 tile the file", to)
	}
	if whole {
		t.Error("wholeFile = true with an unconsumed overlapping fact — the CRC " +
			"describes bytes the file may not hold, and R23 wants unavailable " +
			"rather than a relabelling")
	}
}

// TestVerifiedPrefix_AGaplessFileClaimsWholeFile is the other half of the pin
// above: the guard must not withhold the claim from a file that genuinely tiles.
func TestVerifiedPrefix_AGaplessFileClaimsWholeFile(t *testing.T) {
	facts := []ArticleFact{
		{Offset: 0, Length: 100, CRC32: 0x11111111},
		{Offset: 100, Length: 100, CRC32: 0x22222222},
	}
	all := func(int) bool { return true }
	if to, _, whole := verifiedPrefix(facts, all, 200); !whole || to != 200 {
		t.Errorf("verifiedTo=%d wholeFile=%v, want 200/true", to, whole)
	}
}

// TestVerifiedPrefix_AnUnverifiedFactStopsTheWalk pins that the predicate, not
// just the offsets, bounds the run. This is the clause that differs between the
// two callers — Barrier passes a durable-bitmap lookup, Resumer a precomputed
// slice — so it is the one a closure refactor could silently drop.
func TestVerifiedPrefix_AnUnverifiedFactStopsTheWalk(t *testing.T) {
	facts := []ArticleFact{
		{Offset: 0, Length: 100, CRC32: 0x11111111},
		{Offset: 100, Length: 100, CRC32: 0x22222222},
	}
	firstOnly := func(i int) bool { return i == 0 }

	to, _, whole := verifiedPrefix(facts, firstOnly, 200)
	if to != 100 {
		t.Errorf("verifiedTo = %d, want 100 — the second fact is not verified", to)
	}
	if whole {
		t.Error("wholeFile = true while a fact is unverified — the prefix stops " +
			"short of the file's end and cannot describe it")
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			to, crc, whole := verifiedPrefix(tc.facts, func(i int) bool { return tc.verified[i] }, tc.size)
			if to != tc.wantTo || crc != tc.wantCRC || whole != tc.wantWhole {
				t.Errorf("verifiedPrefix = (%d, %#x, %v), want (%d, %#x, %v)",
					to, crc, whole, tc.wantTo, tc.wantCRC, tc.wantWhole)
			}
		})
	}
}
