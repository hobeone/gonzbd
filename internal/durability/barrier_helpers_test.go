package durability

import "testing"

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
	t.Parallel()
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
	t.Parallel()
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
