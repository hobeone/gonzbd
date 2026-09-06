package durability

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// overlapTarget returns the #387 shape for one file, under the run mechanism:
// A0 [0,100), A1 [100,200), X [150,50) — 250 bytes of articles over a 200-byte
// file, so Σ Length exceeds the file by the 50 bytes X wrote over A1.
//
// X shares no start offset with A1, so the assembler's exact-offset collision
// index never sees it and X's bytes overwrite A1's on disk. It also abuts
// nothing, so it gets a run of its own and the file never collapses to a
// single row — which is what withholds the whole-file CRC (§3.5) and routes
// the file to par2.
func overlapTarget() *factGapTarget {
	return &factGapTarget{
		size: 200,
		drained: []WrittenArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x11},
			{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0x22},
			{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 50, CRC32: 0x33},
		},
	}
}

// TestFinalizeFile_ReportsOverlappingDurableArticles pins the report #387 asks
// for: articles claiming the same bytes are surfaced to the user rather than
// only withheld from the whole-file CRC.
//
// The mechanism is §3.3's arithmetic rather than a structural walk over
// adjacent extents: Σ Length greater than the file's size means articles wrote
// over each other, and the excess is exactly how many bytes were lost.
func TestFinalizeFile_ReportsOverlappingDurableArticles(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))
	tgt := overlapTarget()

	b := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	found, err := b.FinalizeFile(ctx, "job-1", 0, tgt)
	if err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	if len(found) != 1 {
		t.Fatalf("FinalizeFile returned %d anomalies, want 1 — the recorded articles "+
			"account for 250 bytes of a 200-byte file and the user is told nothing", len(found))
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
	// The excess, which is what survives the merge. A run folds the articles
	// that abut into one row, so once the record is written there is no longer
	// a PAIR to name — the byte count is the diagnosis that remains.
	if !strings.Contains(found[0].Reason, "50 bytes") {
		t.Errorf("Reason = %q, want it to state the 50-byte excess", found[0].Reason)
	}
}

// TestFinalizeFile_ReportsAnExactOffsetDuplicate pins the OTHER collision
// shape — the one §3.3's arithmetic structurally cannot see.
//
// Two articles claim offset 0. The store keeps one and drops the other, so the
// dropped bytes contribute nothing to Σ Length and it never exceeds the file's
// size: overlapFrom has no evidence and correctly reports nothing. Before the
// commit began returning its drops, that made this case invisible everywhere —
// dispatch.go's UU block describes it, and no code path told anyone.
//
// The report has to come from the commit because the commit is the last moment
// the collision exists. Afterwards the surviving row is indistinguishable from
// one that never had a rival: no gap, no excess, no index naming the article
// that lost.
func TestFinalizeFile_ReportsAnExactOffsetDuplicate(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	// A0 and A2 both at offset 0; A1 abuts A0. 200 bytes of surviving record
	// over a 200-byte file, so Σ Length EQUALS the size and the overlap check
	// is silent by construction.
	tgt := &factGapTarget{
		size: 200,
		drained: []WrittenArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x11},
			{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: 0x22},
			{FileIdx: 0, ArtIdx: 2, Offset: 0, Length: 100, CRC32: 0x33},
		},
	}

	b := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	found, err := b.FinalizeFile(ctx, "job-1", 0, tgt)
	if err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	if len(found) != 1 {
		t.Fatalf("FinalizeFile returned %d anomalies, want 1 — two articles claimed "+
			"offset 0, one was discarded, and Σ Length still equals the file's size "+
			"so nothing else in the barrier can notice", len(found))
	}
	if found[0].FileIdx != 0 {
		t.Errorf("FileIdx = %d, want 0", found[0].FileIdx)
	}
	if !strings.Contains(found[0].Reason, "vol042.rar") {
		t.Errorf("Reason = %q, want it to name the file so a user can act on it",
			found[0].Reason)
	}
	// Both articles, which is what this report can say and the overlap report
	// cannot: the drop has not happened from the record's point of view until
	// the commit lands, so the pair is still in hand.
	for _, want := range []string{"0", "2", "offset 0"} {
		if !strings.Contains(found[0].Reason, want) {
			t.Errorf("Reason = %q, want it to contain %q — naming both articles and "+
				"the contested offset is the whole advantage this report has",
				found[0].Reason, want)
		}
	}

	// Grounding: Σ Length really does equal the size here, so the assertion
	// above cannot be passing because overlapFrom fired instead.
	runs, err := rs.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, r := range runs {
		total += r.Length
	}
	if total != 200 {
		t.Fatalf("stored runs total %d bytes over a 200-byte file; the fixture has "+
			"drifted into the overlap check's reach and no longer isolates the "+
			"exact-offset shape", total)
	}
}

// TestFinalizeFile_DoesNotReportAHoleAsAnOverlap is the other half of the pin,
// and without it the test above cannot distinguish "reports overlaps" from
// "reports whenever the recorded total differs from the file".
//
// A file waiting on an article is the ordinary state of a running download.
// Σ Length BELOW the size is exactly that case, and reporting it as a
// malformed post would put a warning on essentially every job in the queue.
func TestFinalizeFile_DoesNotReportAHoleAsAnOverlap(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))

	// A0 [0,100) then A2 [200,300) — nothing covers [100,200).
	tgt := &factGapTarget{
		size: 300,
		drained: []WrittenArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x11},
			{FileIdx: 0, ArtIdx: 2, Offset: 200, Length: 100, CRC32: 0x22},
		},
	}
	b := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
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

// TestFinalizeFile_ReportsAnOverlapAboveAPermanentHole is a KNOWN DETECTION
// GAP under the run mechanism, and it is skipped rather than deleted so the
// gap is recorded rather than forgotten.
//
// A0 [0,100), nothing covering [100,200), then A2 [200,300) and A3 [250,50).
// The hole is 100 bytes and the overlap is 50, so Σ Length is 250 against a
// 300-byte file — BELOW it, which §3.3 reads as the ordinary incomplete case.
// A hole and an overlap CANCEL under a sum, and no arithmetic over totals can
// separate them. The old prefix walk compared adjacent extents structurally
// and saw the overlap regardless.
//
// Two things bound the loss, and both are checked elsewhere:
//
//   - The file has a gap, so it keeps more than one run AND no single run
//     covers its whole article range — §3.5 withholds the whole-file CRC on
//     either condition alone. #387's actual harm — publishing a CRC over bytes
//     that are not on disk — is closed structurally, by a different guard, and
//     is pinned in app.recordAssembledCRC's tests.
//   - The file is incomplete either way, so par2 fetches recovery volumes and
//     repairs both defects.
//
// What is lost is a WARNING on a file the user is already told is incomplete.
// Do not "fix" this by contorting the check: a detector that could separate
// them would need the structural comparison this design deleted, along with
// the per-article record it walked.
func TestFinalizeFile_ReportsAnOverlapAboveAPermanentHole(t *testing.T) {
	t.Skip("known detection gap: a 100-byte hole and a 50-byte overlap cancel under " +
		"Σ Length, which lands below the file size and reads as the ordinary " +
		"incomplete case. See this test's doc for why the outcome is still bounded.")
}

// TestRun_ReportsAnOverlapWhenNothingWasAcked pins the return that Run's early
// exit used to discard.
//
// The acked==0 path COMMITS and returns a nil error, so it is a path that
// landed and must carry its finding. It is also not a corner case: it is the
// shape a RESUMED job takes for a file that became durable in an earlier
// process. Nothing drains, so no later cycle re-derives anything and
// finalizeCompletedFile never fires for that file in this process either — the
// overlap would be detected on every checkpoint and reported on none.
//
// The second barrier is the restart. A fresh instance has an empty report
// latch, which is what a new process has, so this test cannot pass merely
// because the first Run already reported.
func TestRun_ReportsAnOverlapWhenNothingWasAcked(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))
	tgt := overlapTarget()

	first := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	if _, err := first.Run(ctx, "job-1", tgt); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Nothing left to drain: every article is already recorded.
	tgt.drained = nil
	restarted := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	found, err := restarted.Run(ctx, "job-1", tgt)
	if err != nil {
		t.Fatalf("Run after restart: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("Run returned %d anomalies with nothing acked, want 1 — the commit "+
			"landed, so the finding is not withheld by the failed-cycle rule, and no "+
			"later cycle re-derives it because this file never drains again", len(found))
	}
	if !strings.Contains(found[0].Reason, "vol042.rar") {
		t.Errorf("Reason = %q, want it to name the file", found[0].Reason)
	}
}

// TestRun_RaisesEachOverlapOnce pins the report latch.
//
// An overlap is a property of the PERSISTED runs, so every checkpoint
// re-derives the same finding from the same rows. Without the latch a job with
// one malformed file raises it on every cycle for the rest of the download —
// and because Job.SetWarning holds a single string, each re-raise also
// overwrites whatever warning was written in between, so a stall reason set at
// cycle N is gone by cycle N+1.
func TestRun_RaisesEachOverlapOnce(t *testing.T) {
	ctx := context.Background()
	rs := NewSQLiteRunStore(openTestDB(t))
	tgt := overlapTarget()

	b := NewBarrier(rs, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	first, err := b.Run(ctx, "job-1", tgt)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first Run returned %d anomalies, want 1", len(first))
	}
	for i := 2; i <= 4; i++ {
		again, err := b.Run(ctx, "job-1", tgt)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if len(again) != 0 {
			t.Fatalf("Run %d re-raised %d anomalies: %+v — the same finding would warn "+
				"on every checkpoint and clobber any other warning set in between",
				i, len(again), again)
		}
	}
}

// TestAdmit_LatchesPerJobAndFile pins the latch's KEY, which the two barrier
// tests cannot distinguish because they only ever use one job.
//
// One Barrier serves every job in the process — Run's caller serialises per
// job, not globally — so a latch keyed on the file index alone would let the
// first job's file 0 silence every other job's file 0 for the life of the
// process. Nothing about that failure is visible from a single-job test.
func TestAdmit_LatchesPerJobAndFile(t *testing.T) {
	b := NewBarrier(nil, nil, nil, slog.New(slog.DiscardHandler))

	if got := b.admit("job-1", nil); got != nil {
		t.Errorf("admit(nil) = %+v, want nil", got)
	}

	one := []PostAnomaly{{FileIdx: 0, Reason: "malformed"}}
	if got := b.admit("job-1", one); len(got) != 1 {
		t.Fatalf("first admit returned %d, want 1", len(got))
	}
	if got := b.admit("job-1", one); len(got) != 0 {
		t.Errorf("second admit for the same job and file returned %d, want 0", len(got))
	}
	if got := b.admit("job-2", one); len(got) != 1 {
		t.Errorf("admit for a DIFFERENT job's file 0 returned %d, want 1 — one job's "+
			"finding would silence every other job's file 0 for the life of the "+
			"process", len(got))
	}
	// A second file of a job that already reported one is a separate finding.
	if got := b.admit("job-1", []PostAnomaly{{FileIdx: 1, Reason: "malformed"}}); len(got) != 1 {
		t.Errorf("admit for the same job's file 1 returned %d, want 1", len(got))
	}
}

// TestForgetJob_LetsARetryWarnAgain pins the latch reset.
//
// A retry re-enters the queue under the SAME job ID and its durable runs
// usually survive with it, so the overlap is re-derived and matches the latch
// from the previous attempt. Without ForgetJob the second attempt is silent —
// and silence is indistinguishable from a healthy download, which is the
// failure mode the warning exists to prevent.
func TestForgetJob_LetsARetryWarnAgain(t *testing.T) {
	b := NewBarrier(nil, nil, nil, slog.New(slog.DiscardHandler))
	one := []PostAnomaly{{FileIdx: 0, Reason: "malformed"}}

	if got := b.admit("job-1", one); len(got) != 1 {
		t.Fatalf("first admit returned %d, want 1", len(got))
	}
	b.ForgetJob("job-1")
	if got := b.admit("job-1", one); len(got) != 1 {
		t.Errorf("admit after ForgetJob returned %d, want 1 — a retried job would "+
			"never warn about an overlap its previous attempt already reported", len(got))
	}

	// Scoped to the job named. A retry of one job must not un-silence every
	// other job in the process, which would restore the per-checkpoint spam
	// the latch exists to stop.
	if got := b.admit("job-2", one); len(got) != 1 {
		t.Fatalf("setup: job-2's first admit returned %d, want 1", len(got))
	}
	b.ForgetJob("job-1")
	if got := b.admit("job-2", one); len(got) != 0 {
		t.Errorf("ForgetJob(job-1) also cleared job-2, returning %d, want 0", len(got))
	}

	// Idempotent, and harmless for a job that never reported.
	b.ForgetJob("job-1")
	b.ForgetJob("never-seen")
}

// TestOverlapReason_StatesTheExcessAndTheFile pins the rendering directly, so
// the message's content is fixed by something other than the barrier tests
// that merely grep it. It is the only test that calls overlapReason with
// arguments of its own choosing, which is what makes the argument ORDER —
// path, recorded, size — a pinned part of the contract rather than an accident
// that happens to read correctly at the single call site.
//
// It replaced a pin on the article PAIR the report used to name. A run merges
// the articles that abut into one row, so by the time the record is written
// there is no pair left to name; the excess byte count is what survives.
func TestOverlapReason_StatesTheExcessAndTheFile(t *testing.T) {
	got := overlapReason("/downloads/movie/vol042.rar", 250, 200)
	for _, want := range []string{"vol042.rar", "250", "200", "50 bytes"} {
		if !strings.Contains(got, want) {
			t.Errorf("overlapReason() = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "/downloads/") {
		t.Errorf("overlapReason() = %q, want the base name only — the full path is "+
			"noise in a user-facing warning", got)
	}
}
