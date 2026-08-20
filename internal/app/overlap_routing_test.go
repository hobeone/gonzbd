package app

import (
	"context"
	"log/slog"
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
	facts := durability.NewSQLiteFactLog(repo.DB())
	if err := facts.Append(ctx, job.ID, []durability.ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100},
		{FileIdx: 0, ArtIdx: 1, Offset: 100, Length: 100},
		{FileIdx: 0, ArtIdx: 2, Offset: 150, Length: 50},
	}); err != nil {
		t.Fatalf("Append facts: %v", err)
	}
	application.barrier = durability.NewBarrier(
		facts, durability.NewSQLiteExtentStore(repo.DB()),
		application.queue, application, slog.New(slog.DiscardHandler))

	return application, job.ID
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
}
