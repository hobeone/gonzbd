package durability

import (
	"bytes"
	"context"
	"hash/crc32"
	"log/slog"
	"testing"
)

// TestFinalizeFile_CollapsesACleanFileToOneRunCarryingTheWholeFileCRC pins the
// substrate the whole-file CRC is read off.
//
// The assembler used to combine the per-article CRCs it happened to see, which
// was #349: a resumed run is never sent the articles an earlier run completed,
// so its parts do not tile the file and the combined value described a
// fragment while claiming to describe the whole.
//
// A run record has no such blind spot. Articles that abut in both offset and
// article index merge into one row as they arrive, across restarts, and their
// CRCs are combined left-to-right by crc32util.Combine — so a file whose
// articles all arrive collapses to a SINGLE row at offset 0 whose crc32 is the
// whole-file CRC, already computed and on stable storage.
//
// The PREDICATE that reads it — exactly one row, offset 0 — lives in
// Application.recordAssembledCRC and is pinned there, including against the
// overlapped file this mechanism must keep at more than one row.
func TestFinalizeFile_CollapsesACleanFileToOneRunCarryingTheWholeFileCRC(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	a0 := bytes.Repeat([]byte{0x01}, 100)
	a1 := bytes.Repeat([]byte{0x02}, 100)
	want := crc32.ChecksumIEEE(append(append([]byte{}, a0...), a1...))

	tgt := &factGapTarget{
		size: 200,
		drained: []WrittenArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a0)},
			{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a1)},
		},
	}
	b := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	if _, err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	stored, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("a file whose articles tile it holds %d runs, want 1 — the predicate "+
			"that publishes the whole-file CRC is a row COUNT, so more than one row "+
			"means no CRC is published at all: %+v", len(stored), stored)
	}
	if stored[0].Offset != 0 {
		t.Errorf("the single run starts at %d, want 0", stored[0].Offset)
	}
	if stored[0].CRC32 != want {
		t.Errorf("run CRC32 = %#x, want %#x (CRC32 of the whole file)", stored[0].CRC32, want)
	}
}

// TestFinalizeFile_AHoleKeepsTheFileAtMoreThanOneRun pins the R23 side of the
// same mechanism: "unavailable" must stay distinguishable from a CRC of zero,
// and a file with a permanently failed article has no whole-file value.
//
// It is structural rather than a flag. A hole is a gap between rows by
// definition, so such a file cannot collapse to one, so the row-count
// predicate withholds the CRC with nothing to remember.
func TestFinalizeFile_AHoleKeepsTheFileAtMoreThanOneRun(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	// Article 1 permanently failed: nothing was ever written for [100,200).
	tgt := &factGapTarget{
		size: 300,
		drained: []WrittenArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x11},
			{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: 0x22},
		},
	}
	b := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	if _, err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	stored, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("a file with a hole holds %d runs, want 2 — one either side of the "+
			"hole. A single row would publish a whole-file CRC over bytes that are "+
			"not there: %+v", len(stored), stored)
	}
}

// TestFinalizeFile_AnOverlapKeepsTheFileAtMoreThanOneRun is #387's structural
// closure, at the layer that produces it.
//
// A0 [0,100), A1 [100,200) tile the file into one merged run. X [150,200) is
// displaced: it abuts nothing, so it cannot merge, so the file keeps a second
// row — which is what the row-count predicate reads to withhold the CRC.
//
// Under the SPAN form of the predicate ("a row starts at 0 and its length
// equals the maximum") this exact shape publishes a CRC combined from the
// ORIGINAL articles while foreign bytes occupy 150-200. par2 then matches a
// manifest whose bytes are not on disk and never fetches the recovery volumes.
// The row count is what carries the guarantee; this pins that the mechanism
// really does leave a second row for it to see.
func TestFinalizeFile_AnOverlapKeepsTheFileAtMoreThanOneRun(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	tgt := &factGapTarget{
		size: 200,
		drained: []WrittenArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x11},
			{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0x22},
			{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 50, CRC32: 0x33},
		},
	}
	b := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	if _, err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	stored, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("an overlapped file holds %d runs, want 2 — the displaced article "+
			"abuts nothing and must keep a row of its own: %+v", len(stored), stored)
	}
	// The merged row spans the whole file, which is exactly why the SPAN form
	// of the predicate would publish here and the ROW-COUNT form does not.
	if stored[0].Offset != 0 || stored[0].Length != 200 {
		t.Errorf("the merged run is [%d,%d), want [0,200) — without it this fixture "+
			"does not exercise the span form the row count exists to beat",
			stored[0].Offset, stored[0].Offset+stored[0].Length)
	}
}
