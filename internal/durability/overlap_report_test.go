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

	tgt := overlapJob(t, ctx, facts)
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

// overlapJob appends the #387 fact shape for one file and returns a target
// over it: A0 [0,100), A1 [100,200), X [150,200), all drained, file 200 bytes.
func overlapJob(t *testing.T, ctx context.Context, facts FactLog) *factGapTarget {
	t.Helper()
	a := bytes.Repeat([]byte{0x01}, 100)
	if err := facts.Append(ctx, "job-1", []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: crc32.ChecksumIEEE(a)},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100, CRC32: crc32.ChecksumIEEE(a)},
		{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 50, CRC32: crc32.ChecksumIEEE(a)},
	}); err != nil {
		t.Fatal(err)
	}
	return &factGapTarget{
		artCount: 3,
		size:     200,
		drained: []WrittenArticle{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
			{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100},
			{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 50},
		},
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
	b := NewBarrier(nil, nil, nil, nil, slog.New(slog.DiscardHandler))

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
// A retry re-enters the queue under the SAME job ID and its Class A facts
// survive with it, so the overlap is re-derived and matches the latch from the
// previous attempt. Without ForgetJob the second attempt is silent — and
// silence is indistinguishable from a healthy download, which is the failure
// mode the warning exists to prevent.
func TestForgetJob_LetsARetryWarnAgain(t *testing.T) {
	b := NewBarrier(nil, nil, nil, nil, slog.New(slog.DiscardHandler))
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

// TestRun_ReportsAnOverlapWhenNothingWasAcked pins the return that Run's
// early exit used to discard.
//
// The acked==0 branch COMMITS and returns a nil error, so it is a path that
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
	db := openTestDB(t)
	facts, exts := NewSQLiteFactLog(db), NewSQLiteExtentStore(db)
	tgt := overlapJob(t, ctx, facts)

	first := NewBarrier(facts, exts, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
	if _, err := first.Run(ctx, "job-1", tgt); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Nothing left to drain: every article is already durable and committed.
	tgt.drained = nil
	restarted := NewBarrier(facts, exts, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
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
// An overlap is a property of PERSISTED facts, so every checkpoint re-derives
// the same finding from the same rows. Without the latch a job with one
// malformed file raises it on every cycle for the rest of the download — and
// because Queue.SetWarning holds a single string, each re-raise also overwrites
// whatever warning was written in between, so a stall reason set at cycle N is
// gone by cycle N+1.
func TestRun_RaisesEachOverlapOnce(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	facts, exts := NewSQLiteFactLog(db), NewSQLiteExtentStore(db)
	tgt := overlapJob(t, ctx, facts)

	b := NewBarrier(facts, exts, &recordingAcker{}, &recordingStall{}, slog.New(slog.DiscardHandler))
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

// TestOverlapFrom_ReportsTheIntersectionNotTheVictimsEnd pins the range the
// report names when the arrival is CONTAINED in the article it landed inside.
//
// The two spans are the same whenever the arrival extends past the victim, so
// the fixture has to contain it to tell them apart: a 10-byte article inside a
// 200-byte one disputes [50,60), not [50,200). Reporting to the victim's end
// hands a user comparing this against a par2 repair log a range fifteen times
// too wide, and names a stretch of the file nothing overwrote.
func TestOverlapFrom_ReportsTheIntersectionNotTheVictimsEnd(t *testing.T) {
	facts := []ArticleFact{
		{ArtIdx: 0, Offset: 0, Length: 200},
		{ArtIdx: 1, Offset: 50, Length: 10},
	}
	w := verifiedPrefix(facts, func(int) bool { return true })
	pa, ok := overlapFrom(w, 0, func() string { return "/downloads/movie/vol042.rar" })
	if !ok {
		t.Fatal("overlapFrom found nothing for a contained overlap")
	}
	if !strings.Contains(pa.Reason, "[50,60)") {
		t.Errorf("Reason = %q, want the intersection [50,60) — article #1 describes "+
			"ten bytes, and reporting to #0's end claims 150 bytes were overwritten "+
			"when 10 were", pa.Reason)
	}
}

// TestOverlapReason_NamesBothArticlesAndTheFile pins the rendering directly, so
// the message's content is fixed by something other than the two barrier tests
// that merely grep it. It is the only test that calls overlapReason with
// arguments of its own choosing, which is what makes the argument ORDER — path,
// first, second, at, end — a pinned part of the contract rather than an
// accident that happens to read correctly at the single call site.
func TestOverlapReason_NamesBothArticlesAndTheFile(t *testing.T) {
	got := overlapReason("/downloads/movie/vol042.rar", 7, 9, 100, 150)
	for _, want := range []string{"vol042.rar", "#7", "#9", "[100,150)"} {
		if !strings.Contains(got, want) {
			t.Errorf("overlapReason() = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "/downloads/") {
		t.Errorf("overlapReason() = %q, want the base name only — the full path is "+
			"noise in a user-facing warning", got)
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
