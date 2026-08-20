package durability

import (
	"bytes"
	"context"
	"hash/crc32"
	"log/slog"
	"testing"
)

// TestFinalizeFile_AnOverlappingFactWithholdsTheWholeFileCRC is the
// barrier-level pin for #387, and it is the one verifiedPrefix's own unit tests
// cannot give: those prove the function is right, not that FinalizeFile asks it.
//
// The facts tile [0,200) from A0 and A1, and X claims [150,200) — inside A1's
// range, sharing no start offset, so the assembler does not detect it and X's
// bytes overwrite A1's on disk. The file does not grow, so VerifiedTo reaches
// Size and the old rule — VerifiedTo > 0 && VerifiedTo == Size, recomputed at
// the call site — reported a whole-file CRC.
//
// That CRC is combined from the facts, and facts are built from each article's
// own decoded bytes before the write. So it describes the file that SHOULD have
// been written, which is exactly what par2 describes. QuickCheck matched it,
// the repair stage skipped par2 entirely, and on-demand par2 declined to
// download the recovery volumes. The corruption was not merely undetected; it
// was unrepairable.
//
// Withholding the claim restores the loud path: app.durability records no CRC,
// QuickCheck reports NoCRC rather than a match, and repair runs.
func TestFinalizeFile_AnOverlappingFactWithholdsTheWholeFileCRC(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	exts := NewSQLiteExtentStore(db)

	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 100)
	x := bytes.Repeat([]byte{0x03}, 50)

	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1)},
		{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 50, CRC32: crc32.ChecksumIEEE(x)},
	}); err != nil {
		t.Fatal(err)
	}

	tgt := &factGapTarget{
		artCount: 3,
		size:     200,
		drained: []WrittenArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
			{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100},
			{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 50},
		},
	}
	b := NewBarrier(facts, exts, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	if err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	stored, err := exts.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("Load returned %d extents, want 1", len(stored))
	}
	got := stored[0]

	// Grounding: without this, the assertion below could pass because the walk
	// stopped early for some unrelated reason, which would prove nothing about
	// the overlap.
	if got.VerifiedTo != 200 {
		t.Fatalf("VerifiedTo = %d, want 200 — A0 and A1 must tile the file for this "+
			"fixture to exercise the case where the prefix reaches Size and a fact "+
			"is still left over", got.VerifiedTo)
	}
	if got.HasPrefixCRC {
		t.Errorf("HasPrefixCRC = true with X's fact unconsumed: the barrier published "+
			"a whole-file CRC (%#x) combined only from A0 and A1, describing the bytes "+
			"that SHOULD be on disk. That value matches par2's, so QuickCheck reports "+
			"clean, repair is skipped and the recovery volumes are never fetched (#387)",
			got.PrefixCRC)
	}
}
