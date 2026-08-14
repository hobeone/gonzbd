package durability

import (
	"bytes"
	"context"
	"hash/crc32"
	"log/slog"
	"testing"
)

// TestFinalizeFile_ProducesAWholeFileCRC pins the CRC that QuickCheck and
// on-demand par2 consume.
//
// The assembler used to combine the per-article CRCs it happened to see, which
// was #349: a resumed run is never sent the articles an earlier run completed,
// so its parts do not tile the file and the combined value described a
// fragment while claiming to describe the whole. That writer was removed and
// nothing replaced it, so assembled_crc32 was 0 for every download.
//
// FileExtent.PrefixCRC was named as the replacement but could not be one. The
// barrier never WROTE a PrefixCRC — buildExtent only ever cleared it, and only
// Resumer set it, at startup — so a file downloaded without a restart had no
// CRC at any point in its life.
//
// Class A is the right source, and it is the source #349's combine lacked: the
// facts persist across restarts, so they name every article of the file
// regardless of which run fetched it. Combining them is arithmetic over rows
// the barrier has already loaded, with no read of the file.
func TestFinalizeFile_ProducesAWholeFileCRC(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	exts := NewSQLiteExtentStore(db)

	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 100)
	want := crc32.ChecksumIEEE(append(append([]byte{}, a0...), a1...))

	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1)},
	}); err != nil {
		t.Fatal(err)
	}

	tgt := &factGapTarget{
		artCount: 2,
		size:     200,
		drained: []WrittenArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
			{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100},
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

	// Grounding: the facts must actually tile the file, or "no CRC" would be
	// the correct answer and this test would be asserting the wrong thing.
	if got.VerifiedTo != 200 {
		t.Fatalf("VerifiedTo = %d, want 200; the fixture's facts do not tile the "+
			"file, so a whole-file CRC is legitimately unavailable", got.VerifiedTo)
	}
	if !got.HasPrefixCRC {
		t.Fatal("a completed file whose facts tile it end to end reports no whole-file " +
			"CRC. QuickCheck can never bypass the full par2 verify and on-demand par2 " +
			"fetches every recovery volume even for a bit-perfect download")
	}
	if got.PrefixCRC != want {
		t.Errorf("PrefixCRC = %#x, want %#x (CRC32 of the whole file)", got.PrefixCRC, want)
	}
}

// TestFinalizeFile_NoWholeFileCRCWhenAnArticleIsMissing pins the other side:
// R23 asks for "unavailable" to stay distinguishable from a CRC of zero, and a
// file with a permanently failed article has no whole-file value to report.
func TestFinalizeFile_NoWholeFileCRCWhenAnArticleIsMissing(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	exts := NewSQLiteExtentStore(db)

	a1 := bytes.Repeat([]byte{0x02}, 100)
	// Article 0 permanently failed, so it never decoded and wrote no fact.
	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1)},
	}); err != nil {
		t.Fatal(err)
	}

	tgt := &factGapTarget{
		artCount: 2,
		size:     200,
		drained:  []WrittenArticle{{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100}},
	}
	b := NewBarrier(facts, exts, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	if err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	stored, err := exts.Load(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored[0].HasPrefixCRC {
		t.Errorf("a file with a hole at its start reported a whole-file CRC of %#x; "+
			"the prefix walk stops at the hole, so that value covers no bytes at all "+
			"and QuickCheck would compare it against par2's real one", stored[0].PrefixCRC)
	}
}
