package durability

import (
	"context"
	"errors"
	"testing"
)

// TestBoundOver_TakesTheMaximumAcrossBothSources pins the truncate bound's
// arithmetic directly, at the level FinalizeFile cannot reach: the two sources
// are the file's STORED runs and the articles this drain is about to add, and
// a bound that consulted only one of them is destructive in a different way
// for each.
//
// Only-stored discards whatever this drain just wrote. Only-drained is the
// #342/#350 shape: on a resumed file it sits below what earlier runs wrote and
// truncating to it destroys them. The fixture makes the two maxima different
// numbers so neither mistake can pass.
func TestBoundOver_TakesTheMaximumAcrossBothSources(t *testing.T) {
	stored := []Run{{Offset: 0, Length: 100}, {Offset: 400, Length: 100}}
	arts := []DurableArticle{{Offset: 100, Length: 100}}

	if got := boundOver(stored, arts); got != 500 {
		t.Errorf("boundOver = %d, want 500 — 200 would be this drain's high-water mark "+
			"and would destroy the stored run at [400,500)", got)
	}
	// The drain reaching higher than anything stored is the other direction,
	// and it is the ordinary case for a file's last articles.
	if got := boundOver(stored, []DurableArticle{{Offset: 500, Length: 100}}); got != 600 {
		t.Errorf("boundOver = %d, want 600", got)
	}
	// Neither source is the bound == 0 branch: a file with nothing recorded
	// and nothing drained must not be truncated to zero.
	if got := boundOver(nil, nil); got != 0 {
		t.Errorf("boundOver(nil, nil) = %d, want 0", got)
	}
}

// TestDurableArticle_CarriesEveryFieldTheRunNeeds pins the one conversion
// between what a drain reports and what the store places.
//
// It exists as a function rather than two inline literals precisely so the two
// commit sites cannot disagree about it, and a dropped field here is silent:
// a zero CRC32 hashes to zero rather than reading as "unknown", and a zero
// Offset places the run at the start of the file.
func TestDurableArticle_CarriesEveryFieldTheRunNeeds(t *testing.T) {
	got := durableArticle(7, WrittenArticle{
		FileIdx: 99, ArtIdx: 12, Offset: 4096, Length: 100, CRC32: 0xC0FFEE,
	})
	want := DurableArticle{FileIdx: 7, ArtIdx: 12, Offset: 4096, Length: 100, CRC32: 0xC0FFEE}
	if got != want {
		t.Errorf("durableArticle = %+v, want %+v", got, want)
	}
	// The FILE index comes from the barrier's loop, not from the article. The
	// two agree in production, and taking the caller's is what keeps a drain
	// report that disagreed from placing a run in the wrong file's record.
	if got.FileIdx != 7 {
		t.Errorf("FileIdx = %d, want the caller's 7 rather than the article's 99", got.FileIdx)
	}
}

// TestOverlapFindings_SurvivesAnUnreadableRecord pins the one decision this
// helper makes beyond delegating to overlapFrom.
//
// It runs BELOW the commit and the ack, both of which have already landed, so
// a read failure here may not fail the cycle. The finding is a property of
// rows on stable storage, and the next committing checkpoint asks the same
// question of the same rows.
func TestOverlapFindings_SurvivesAnUnreadableRecord(t *testing.T) {
	b := NewBarrier(&errRunStore{err: errors.New("unreadable")}, nil, nil, testLogger(t))

	got := b.overlapFindings(context.Background(), "job-1", []int32{0, 1},
		map[int32]int64{0: 100, 1: 100}, &fakeTarget{})

	if got != nil {
		t.Errorf("findings = %+v from an unreadable record, want none — a read failure "+
			"is not evidence that a file's articles wrote over each other", got)
	}
}

// TestOverlapFindings_ReportsOnePerOverlappedFile pins that it iterates rather
// than stopping at the first file, and that a healthy file beside a malformed
// one contributes nothing.
//
// Run's caller cannot see which of a job's files a finding belongs to — that
// is why PostAnomaly carries FileIdx — so a helper that returned only the
// first would silence every file after it for the life of the process, since
// admit latches per file.
func TestOverlapFindings_ReportsOnePerOverlappedFile(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))
	// File 0 overlaps: 150 bytes recorded over a 100-byte file. File 1 is
	// healthy. File 2 overlaps too.
	if err := rs.Commit(ctx, "job-1", []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
		{FileIdx: 0, ArtIdx: 2, Offset: 50, Length: 50, CRC32: 2},
		{FileIdx: 1, ArtIdx: 3, Offset: 0, Length: 100, CRC32: 3},
		{FileIdx: 2, ArtIdx: 4, Offset: 0, Length: 100, CRC32: 4},
		{FileIdx: 2, ArtIdx: 6, Offset: 60, Length: 40, CRC32: 5},
	}); err != nil {
		t.Fatal(err)
	}
	b := NewBarrier(rs, nil, nil, testLogger(t))

	got := b.overlapFindings(ctx, "job-1", []int32{0, 1, 2},
		map[int32]int64{0: 100, 1: 100, 2: 100}, &fakeTarget{})

	if len(got) != 2 {
		t.Fatalf("findings = %+v, want one for each of files 0 and 2", got)
	}
	if got[0].FileIdx != 0 || got[1].FileIdx != 2 {
		t.Errorf("findings name files %d and %d, want 0 and 2 — the healthy file between "+
			"them must contribute nothing", got[0].FileIdx, got[1].FileIdx)
	}
}
