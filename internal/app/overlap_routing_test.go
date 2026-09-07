package app

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// The barrier's overlap findings are returned rather than pushed through a
// collaborator, which means a caller CAN drop them: `_, err := b.Run(...)`
// compiles and the warning evaporates with no signal. That shape was chosen
// deliberately for explicit data flow, and it was acceptable only because these
// tests exist.
//
// There are two of them because there are two routes. Run and FinalizeFile are
// independent production call sites with independent wiring, so a single test
// would leave whichever one was forgotten unprotected by the very argument that
// justified the design.

// overlapFixture builds a job that WRITES A0 [0,100), A1 [100,200) and X
// [150,300) — X starting inside A1's range and sharing no start offset, which
// is what the assembler's collision index cannot see. All three are drainable,
// so the barrier records all three and Σ Length exceeds the file by 50 bytes.
func overlapFixture(t *testing.T, ctx context.Context) (*Application, string) {
	t.Helper()
	application, repo, _ := newLifecycleTestApp(t)

	if err := application.assembler.Start(ctx); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	t.Cleanup(func() { _ = application.assembler.Stop() })

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject: "overlap.bin",
		Bytes:   300,
		Articles: []nzb.Article{
			{ID: "a0@t", Bytes: 100, Number: 1},
			{ID: "a1@t", Bytes: 100, Number: 2},
			{ID: "a2@t", Bytes: 100, Number: 3},
		},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, "overlap.nzb", types.FetchOptions{NzbName: "overlap"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.dispatcher.Add(j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := application.pipeline.registerFile(j.ID(), 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}

	// Written through the assembler so the barrier can drain them, and the
	// OVERLAP is now in the WRITES rather than in a separate record — which
	// is the whole of what the durable-runs change did to this fixture. The
	// barrier records what the drain reports, so there is no longer a second
	// place for an overlap to live.
	//
	// Articles 0 and 1 tile [0,200). Article 2 spans [150,300): it overlaps
	// article 1 without sharing a start offset, which is #387's shape and
	// which the assembler's exact-offset collision index cannot see, AND it
	// ends where the file does. That last part is load-bearing — FinalizeFile
	// derives its truncate bound from max(offset+length), so an article set
	// stopping at 200 would trim article 2's bytes away and the fixture would
	// quietly exercise a destructive truncate while the test still saw its
	// warning.
	//
	// Σ Length is then 350 against a 300-byte file, so §3.3's check reports a
	// 50-byte excess.
	for _, w := range []struct {
		art    int
		offset int64
		size   int
	}{{0, 0, 100}, {1, 100, 100}, {2, 150, 150}} {
		ref := assembler.ArticleRef{
			JobID: j.ID(), FileIdx: 0,
			ArtIdx:    int32(w.art), //nolint:gosec // G115: test article counts are tiny
			MessageID: string(rune('a'+w.art)) + "@t",
		}
		req := assembler.WriteRequest{Offset: w.offset, Data: make([]byte, w.size)}
		if err := application.assembler.WriteArticle(ctx, ref, req); err != nil {
			t.Fatalf("WriteArticle %d: %v", w.art, err)
		}
	}

	application.barrier = durability.NewBarrier(
		durability.NewSQLiteRunStore(repo.DB()),
		application, application, slog.New(slog.DiscardHandler))

	return application, j.ID()
}

// assertOverlapWarned checks the message, not that a warning exists.
// job.Warning is single-valued with at least five other writers — the stall
// reason, two durability warnings, the claim-failure note, the queue removal
// failure note — and both Application.Stall and Application.Fail set it and
// are reachable from a barrier that failed inside the same call. A non-emptiness
// assertion would pass on a fixture that faulted for an unrelated reason.
func assertOverlapWarned(t *testing.T, application *Application, jobID, route string) {
	t.Helper()
	var warning string
	if row, ok := application.dispatcher.Row(jobID); ok {
		warning = row.Header.Warning
	}
	// The base name and the excess. Not the article indices: a run merges the
	// articles that abut into one row, so by the time the record is written
	// there is no pair left to name — the byte count is the diagnosis that
	// survives. Not the file index either: it reaches the log line only.
	for _, want := range []string{"overlap.bin", "50 bytes"} {
		if !strings.Contains(warning, want) {
			t.Errorf("%s route: job warning = %q, want it to name %q — the file's "+
				"recorded articles account for more bytes than the file holds, so some "+
				"of them wrote over each other and the user is told nothing",
				route, warning, want)
		}
	}
}

// TestReportPostAnomalies_WritesEveryFinding exercises the routing helper
// directly, over the shapes the two end-to-end tests above cannot produce.
//
// A single Run reports at most one overlap per file but iterates a job's files,
// so a job with two malformed files yields two findings in one slice — and
// because Job.Warning is a single string, only the last survives. That is a
// deliberate accepted cost (see handlePostAnomaly), and pinning it here is what
// makes it a decision rather than an accident: if the warning ever becomes a
// list, this test fails and asks the question.
func TestReportPostAnomalies_WritesEveryFinding(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	jobID := addStallTestJob(t, application, "warnable").ID()

	// Empty first: the overwhelmingly common case must not touch the warning.
	application.reportPostAnomalies(jobID, nil)
	row, ok := application.dispatcher.Row(jobID)
	if !ok {
		t.Fatalf("job %s not found in dispatcher", jobID)
	}
	if w := row.Header.Warning; w != "" {
		t.Fatalf("an empty finding list set the warning to %q", w)
	}

	application.reportPostAnomalies(jobID, []durability.PostAnomaly{
		{FileIdx: 0, Reason: "first file is malformed"},
		{FileIdx: 1, Reason: "second file is malformed"},
	})
	row, _ = application.dispatcher.Row(jobID)
	if w := row.Header.Warning; w != "second file is malformed" {
		t.Errorf("job warning = %q, want the LAST finding — Job.Warning holds one "+
			"string, so a second file's report overwrites the first's", w)
	}
}

// TestPostAnomaly_SurvivesAJobThatHasLeftTheQueue pins the drop, which is
// ordinary rather than a defect (A2): a job can be removed between the barrier
// returning a finding and the report being routed, because the report is
// deliberately made after the per-job mutex is released.
func TestPostAnomaly_SurvivesAJobThatHasLeftTheQueue(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.postAnomaly("no-such-job", 0, "barrier", "malformed")
}

func TestCheckpointJob_RoutesAnOverlapToTheJobWarning(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	application, jobID := overlapFixture(t, ctx)
	application.checkpointJob(ctx, jobID)
	assertOverlapWarned(t, application, jobID, "Run")
}

func TestFinalizeFile_RoutesAnOverlapToTheJobWarning(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	application, jobID := overlapFixture(t, ctx)
	if err := application.finalizeCompletedFile(ctx, jobID, 0); err != nil {
		t.Fatalf("finalizeCompletedFile: %v", err)
	}
	assertOverlapWarned(t, application, jobID, "FinalizeFile")

	// The finalize must not have trimmed the file. Checking the warning alone
	// cannot tell a healthy route from one that reported correctly while
	// eating the last article: an overlapped file is exactly the shape where
	// a bound taken over the wrong set stops short of the real end, and a
	// truncate to it destroys bytes that are on disk.
	st, err := os.Stat(application.filePathFor(jobID, 0))
	if err != nil {
		t.Fatalf("stat the finalized file: %v", err)
	}
	if st.Size() != 300 {
		t.Errorf("file is %d bytes after finalize, want 300 — the top article spans "+
			"[150,300), so a smaller file means the truncate bound did not reach the "+
			"highest recorded end", st.Size())
	}
}
