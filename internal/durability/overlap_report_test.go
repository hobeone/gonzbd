package durability

import (
	"bytes"
	"context"
	"hash/crc32"
	"log/slog"
	"strings"
	"testing"
)

// TestFinalizeFile_ReportsOverlappingDurableArticles pins the report #387 asks
// for: two durable articles claiming the same bytes are surfaced to the user
// rather than only withheld from the whole-file CRC.
//
// A0 [0,100), A1 [100,200), X [150,200). X lands inside A1's range without
// sharing its start offset, so the assembler never detects it and X's bytes
// overwrite A1's on disk. #410 stopped the barrier publishing a CRC for that
// file, which routes it to par2 — but nothing told the user why their download
// needed repairing.
func TestFinalizeFile_ReportsOverlappingDurableArticles(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	exts := NewSQLiteExtentStore(db)

	a := bytes.Repeat([]byte{0x01}, 100)
	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a)},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a)},
		{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 50, CRC32: crc32.ChecksumIEEE(a)},
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
	found, err := b.FinalizeFile(ctx, "job-1", 0, tgt)
	if err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	if len(found) != 1 {
		t.Fatalf("FinalizeFile returned %d anomalies, want 1 — two durable articles "+
			"claim [150,200) and the user is told nothing", len(found))
	}
	if found[0].FileIdx != 0 {
		t.Errorf("FileIdx = %d, want 0", found[0].FileIdx)
	}
	// The base name, not the whole string: pinning exact prose makes this a
	// change-detector for wording, while asserting nothing lets "" pass.
	if !strings.Contains(found[0].Reason, "vol042.rar") {
		t.Errorf("Reason = %q, want it to name the file so a user can act on it",
			found[0].Reason)
	}
	// Both articles, because the report is about a pair. Naming only the
	// arrival would read as "this article is bad" when the point is that two
	// of them describe the same bytes.
	for _, want := range []string{"#1", "#2"} {
		if !strings.Contains(found[0].Reason, want) {
			t.Errorf("Reason = %q, want it to name article %s — the report is about "+
				"a pair, and one index alone does not say what collided", found[0].Reason, want)
		}
	}
}

// TestFinalizeFile_DoesNotReportAHoleAsAnOverlap is the other half of the pin,
// and without it the test above cannot distinguish "reports overlaps" from
// "reports whenever the walk stops early".
//
// A file waiting on an article is the ordinary state of a running download.
// Reporting that as a malformed post would put a warning on essentially every
// job in the queue.
func TestFinalizeFile_DoesNotReportAHoleAsAnOverlap(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts := NewSQLiteFactLog(db)
	exts := NewSQLiteExtentStore(db)

	a := bytes.Repeat([]byte{0x01}, 100)
	// A0 [0,100) then a fact at 200 — nothing covers [100,200).
	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a)},
		{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: crc32.ChecksumIEEE(a)},
	}); err != nil {
		t.Fatal(err)
	}

	tgt := &factGapTarget{
		artCount: 3,
		size:     300,
		drained: []WrittenArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
			{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100},
		},
	}
	b := NewBarrier(facts, exts, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	found, err := b.FinalizeFile(ctx, "job-1", 0, tgt)
	if err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("FinalizeFile reported %d anomalies for a file with a HOLE: %+v — "+
			"an incomplete download is not a malformed post, and this would warn on "+
			"every job in the queue", len(found), found)
	}
}
