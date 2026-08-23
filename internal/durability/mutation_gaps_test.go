package durability

import (
	"context"
	"log/slog"
	"testing"
)

// This file holds the boundary cases mutation testing found unpinned. Four of
// its six tests died with the two-record design — three about the durable
// bitmap's word-boundary arithmetic, which has no bitmap left to index, and
// one about the A2/R28 loud failure on a fact whose article index equalled the
// file's article count, which went with SyncTarget.FileLocalOrdinal. The two
// below survive because their boundaries are about the RECORD's arithmetic,
// which the change reshaped rather than removed.

// TestResume_AdoptsARunEndingExactlyAtEndOfFile pins the boundary of §3.4's
// gate: `size >= max(Offset+Length)` must ADOPT on equality.
//
// A run ending exactly at the file's end is the ordinary shape of a completed,
// truncated file — the trim makes the two equal by construction — so an
// off-by-one here would discard the runs of every file that finished cleanly
// and re-download the entire job on the next start.
//
// Nothing else in the resume suite lands on equality: its adopt case is
// deliberately generous and its discard case is one byte short, which leaves
// exactly this value untested by either.
func TestResume_AdoptsARunEndingExactlyAtEndOfFile(t *testing.T) {
	ctx := context.Background()
	path := writePartial(t, t.TempDir(), "f.bin", 100)
	rs := NewSQLiteRunStore(openTestDB(t))
	storeRuns(t, rs, "job-1",
		DurableArticle{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1})
	r := NewResumer(rs, testLogger(t))

	res, err := r.Resume(ctx, "job-1", 0, path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restart {
		t.Fatal("a run ending exactly at the file's end was discarded; a `>` where the " +
			"gate needs `>=` re-downloads every completed file on every start")
	}
	if len(res.Runs) != 1 {
		t.Fatalf("adopted %d runs, want 1", len(res.Runs))
	}
	if got, _ := rs.ForFile(ctx, "job-1", 0); len(got) != 1 {
		t.Errorf("the store holds %d runs after an adopt, want 1", len(got))
	}
}

// TestFinalizeFile_ARunAtOffsetZeroOfZeroLengthDoesNotTruncate pins the other
// side of the bound == 0 branch.
//
// TestFinalizeFile_NothingRecordedDoesNotTruncate reaches it with no rows at
// all, which a `len(stored) == 0` check would satisfy just as well as the
// arithmetic does. This reaches it with a row PRESENT whose end offset is
// still 0, so the branch has to be reading max(Offset+Length) rather than
// counting rows. A zero-length article is not a state the downloader produces,
// which is exactly why nothing else covers this input.
func TestFinalizeFile_ARunAtOffsetZeroOfZeroLengthDoesNotTruncate(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))
	if err := rs.Commit(ctx, "job-1", []DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 0, CRC32: 0},
	}); err != nil {
		t.Fatal(err)
	}
	tgt := &truncTarget{}

	b := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	if _, err := b.FinalizeFile(ctx, "job-1", 0, tgt); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}
	if tgt.called {
		t.Errorf("truncated to %d for a record whose highest end offset is 0; the file "+
			"would be cut to nothing", tgt.bound)
	}
}
