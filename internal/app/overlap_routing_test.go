package app

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
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

// overlapFixture builds a job whose facts describe A0 [0,100), A1 [100,200) and
// X [150,200) — X inside A1's range, sharing no start offset, which is what the
// assembler cannot see. All three are written and therefore drainable, so the
// barrier's durable predicate accepts them and the walk reaches the overlap.
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
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "overlap"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.queue.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := application.pipeline.registerFile(job.ID, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}

	// Written through the assembler so the barrier can drain them, at offsets
	// that tile: the OVERLAP lives in the facts, which is where the barrier
	// looks. Driving a real overlapping write here would exercise the
	// assembler's blindness rather than this routing.
	for i := range 3 {
		ref, req := assemblerWrite(job.ID, 0, i, int64(i)*100)
		if err := application.assembler.WriteArticle(ctx, ref, req); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}

	// Facts by hand: this fixture writes through the assembler directly and so
	// never passes through pipeline.appendArticleFacts, which is what would
	// normally record them.
	//
	// The facts must reach the file's real end, 300. FinalizeFile derives its
	// truncate bound from them, so a fact set stopping at 200 would trim away
	// article 2's bytes — the test would still see its warning, while the
	// fixture quietly exercised a destructive truncate. Article 2's fact
	// therefore spans [150,300): it overlaps article 1 without sharing a start
	// offset, which is #387's shape, AND ends where the file does.
	facts := durability.NewSQLiteFactLog(repo.DB())
	if err := facts.Append(ctx, job.ID, []durability.ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100},
		{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 150},
	}); err != nil {
		t.Fatalf("Append facts: %v", err)
	}
	application.barrier = durability.NewBarrier(
		facts, durability.NewSQLiteExtentStore(repo.DB()),
		application.queue, application, slog.New(slog.DiscardHandler))

	return application, job.ID
}

// addWarnableJob adds a minimal job that can carry a warning. It needs no
// assembler and no facts: the helpers under test take the finding as an
// argument rather than deriving it.
func addWarnableJob(t *testing.T, application *Application) string {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "warnable.bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: "w0@t", Bytes: 100, Number: 1}},
	}}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "warnable"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.queue.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return job.ID
}

// assertOverlapWarned checks the message, not that a warning exists.
// job.Warning is single-valued with at least four other writers — the stall
// reason, two durability warnings, the claim-failure note — and both
// Application.Stall and Application.Fail set it and are reachable from a
// barrier that failed inside the same call. A non-emptiness assertion would
// pass on a fixture that faulted for an unrelated reason.
func assertOverlapWarned(t *testing.T, application *Application, jobID, route string) {
	t.Helper()
	var warning string
	if j := application.queue.SnapshotJob(jobID); j != nil {
		warning = j.Warning
	}
	// The base name and both article indices. Not the file index: it reaches
	// the log line only, never the warning.
	for _, want := range []string{"overlap.bin", "#1", "#2"} {
		if !strings.Contains(warning, want) {
			t.Errorf("%s route: job warning = %q, want it to name %q — two durable "+
				"articles describe the same bytes and the user is told nothing",
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
	application, _, _ := newLifecycleTestApp(t)
	jobID := addWarnableJob(t, application)

	// Empty first: the overwhelmingly common case must not touch the warning.
	application.reportPostAnomalies(jobID, nil)
	if w := application.queue.SnapshotJob(jobID).Warning; w != "" {
		t.Fatalf("an empty finding list set the warning to %q", w)
	}

	application.reportPostAnomalies(jobID, []durability.PostAnomaly{
		{FileIdx: 0, Reason: "first file is malformed"},
		{FileIdx: 1, Reason: "second file is malformed"},
	})
	if w := application.queue.SnapshotJob(jobID).Warning; w != "second file is malformed" {
		t.Errorf("job warning = %q, want the LAST finding — Job.Warning holds one "+
			"string, so a second file's report overwrites the first's", w)
	}
}

// TestPostAnomaly_SurvivesAJobThatHasLeftTheQueue pins the drop, which is
// ordinary rather than a defect (A2): a job can be removed between the barrier
// returning a finding and the report being routed, because the report is
// deliberately made after the per-job mutex is released.
func TestPostAnomaly_SurvivesAJobThatHasLeftTheQueue(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	application.postAnomaly("no-such-job", 0, "barrier", "malformed")
}

func TestCheckpointJob_RoutesAnOverlapToTheJobWarning(t *testing.T) {
	ctx := t.Context()
	application, jobID := overlapFixture(t, ctx)
	application.checkpointJob(ctx, jobID)
	assertOverlapWarned(t, application, jobID, "Run")
}

func TestFinalizeFile_RoutesAnOverlapToTheJobWarning(t *testing.T) {
	ctx := t.Context()
	application, jobID := overlapFixture(t, ctx)
	if err := application.finalizeCompletedFile(ctx, jobID, 0); err != nil {
		t.Fatalf("finalizeCompletedFile: %v", err)
	}
	assertOverlapWarned(t, application, jobID, "FinalizeFile")

	// The finalize must not have trimmed the file. Asserted because this
	// fixture supplies its facts by hand, so nothing else keeps them
	// consistent with what was written: a fact set stopping short of the
	// file's end produces a truncate that destroys real bytes, and the warning
	// assertion above would pass anyway. Checking the warning alone cannot
	// tell a healthy route from one that reported correctly while eating the
	// last article.
	st, err := os.Stat(application.filePathFor(jobID, 0))
	if err != nil {
		t.Fatalf("stat the finalized file: %v", err)
	}
	if st.Size() != 300 {
		t.Errorf("file is %d bytes after finalize, want 300 — three 100-byte "+
			"articles were written at 0, 100 and 200, so a smaller file means the "+
			"truncate bound was derived from facts that do not reach the end",
			st.Size())
	}
}
