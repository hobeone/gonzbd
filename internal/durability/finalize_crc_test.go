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
	if _, err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
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
	if _, err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
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

// TestGaplessPrefixCRC_EmptyFileClaimsNoWholeFileCRC pins the one input on
// which both of the walk's clauses are satisfied by there being nothing to
// check: a zero-length file with no recorded facts.
//
// The run consumes every fact (there are none) and reaches the file's end (it
// is 0), so the flag came back true with a CRC of 0. CRC32 of zero bytes is
// genuinely 0, which is what makes this the wrong kind of wrong — the value is
// not fabricated, the CLAIM is. HasPrefixCRC means "this is a verified
// whole-file CRC", and a file with no facts has been verified against nothing.
//
// It matters because a target file exists before its first article lands: the
// assembler creates it, and with pre-allocation off it is zero-length until a
// write. A resume in that window would commit a whole-file CRC for it, and
// QuickCheck compares exactly this flag's value against par2's hash — turning
// a job that has merely not started into one reported as damaged.
//
// Both paths now ask verifiedPrefix, so this pins the rule once for the
// barrier and the resume alike. It used to pin only the resume copy, which is
// how the two came to disagree about a different clause (#387).
func TestVerifiedPrefix_EmptyFileClaimsNoWholeFileCRC(t *testing.T) {
	w := verifiedPrefix(nil, func(int) bool { return true })
	verifiedTo, crc, whole := w.VerifiedTo, w.PrefixCRC, w.wholeFile(0)
	if whole {
		t.Error("verifiedPrefix reported a verified whole-file CRC for a zero-length " +
			"file with no facts; QuickCheck would compare 0 against par2's real hash " +
			"and report an untouched job as damaged")
	}
	if verifiedTo != 0 || crc != 0 {
		t.Errorf("verifiedTo/crc = %d/%d, want 0/0 — the prefix itself is unchanged, "+
			"only the claim about it", verifiedTo, crc)
	}
}
