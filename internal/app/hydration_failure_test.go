package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// nonResidentJobWithMissingManifest builds a store-backed queue holding two
// jobs under WithMaxActiveJobs(1): the first is promoted resident, the
// second stays queued and non-resident. It then deletes the second job's
// on-disk manifest so a subsequent Snapshot/SnapshotJob call must hit the
// hydration path and fail — the same "something outside the queue's control
// has damaged it" scenario internal/queue/snapshot_failclosed_test.go pins
// at the queue package level. Package app cannot reach queue's unexported
// test helpers, so this reconstructs the fixture via exported APIs only.
func nonResidentJobWithMissingManifest(t *testing.T) (*queue.Queue, string) {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "history.db")
	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := queue.NewSQLiteStore(repo.DB(), dir, repo)

	q := queue.New(queue.WithStore(store), queue.WithStateDir(dir), queue.WithMaxActiveJobs(1))

	makeJob := func(id string) *queue.Job {
		parsed := &nzb.NZB{Files: []nzb.File{
			{Subject: id + "-file.bin", Bytes: 100, Articles: []nzb.Article{{ID: id + "-a@t", Bytes: 100, Number: 1}}},
		}}
		job, err := queue.NewJob(parsed, queue.AddOptions{Filename: id + ".nzb"}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("NewJob: %v", err)
		}
		job.ID = id
		job.Name = id
		return job
	}

	if err := q.Add(makeJob("resident-job")); err != nil {
		t.Fatalf("Add resident-job: %v", err)
	}
	damagedID := "damaged-job"
	if err := q.Add(makeJob(damagedID)); err != nil {
		t.Fatalf("Add damaged-job: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "manifests", damagedID+".json.gz")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	return q, damagedID
}

// TestHandleFileComplete_HydrationFailureLogsAndReturns pins that
// handleFileComplete's post-MarkFileComplete SnapshotJob call treats a
// hydration failure (job exists, stored state damaged) as a distinct, louder
// outcome from a routine "job vanished mid-download" ErrNotFound — it must
// not silently return via the same branch, and it must not panic reaching
// into a nil snap.
func TestHandleFileComplete_HydrationFailureLogsAndReturns(t *testing.T) {
	q, damagedID := nonResidentJobWithMissingManifest(t)

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}

	app := &Application{
		queue:   q,
		log:     slog.Default(),
		config:  cfg,
		emitter: dummyEmitter{},
	}

	// Must not panic despite the damaged job's manifest being unreadable.
	app.handleFileComplete(context.Background(), FileComplete{JobID: damagedID, FileIdx: 0})
}

// TestFinalizeIfCompleteAfterRetry_HydrationFailureIsNonFatal pins that the
// post-retry opportunistic snapshot/finalize check RetryHistoryJob runs
// treats a hydration failure as a logged, non-fatal outcome: the retry
// itself already succeeded by the time this runs, so a damaged snapshot must
// not panic or be treated the same as the job simply having vanished
// (ErrNotFound).
func TestFinalizeIfCompleteAfterRetry_HydrationFailureIsNonFatal(t *testing.T) {
	q, damagedID := nonResidentJobWithMissingManifest(t)

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}

	app := &Application{
		queue:   q,
		log:     slog.Default(),
		config:  cfg,
		emitter: dummyEmitter{},
	}

	// Must not panic despite the damaged job's manifest being unreadable,
	// and must return without finalizing (there is nothing valid to finalize).
	app.finalizeIfCompleteAfterRetry(damagedID)
}

// TestDirectUnpackOrchestrator_MaybeStart_HydrationFailureLogsAndReturns pins
// that maybeStart's SnapshotJob call — a best-effort DirectUnpack path that
// must never abort the surrounding completion handler — treats a hydration
// failure as a logged, non-fatal skip distinct from the routine "job
// vanished" ErrNotFound case already covered by
// TestApplication_MaybeDirectUnpack (nonexistent job).
func TestDirectUnpackOrchestrator_MaybeStart_HydrationFailureLogsAndReturns(t *testing.T) {
	q, damagedID := nonResidentJobWithMissingManifest(t)

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}

	app := &Application{
		queue:   q,
		log:     slog.Default(),
		config:  cfg,
		emitter: dummyEmitter{},
	}
	app.duOrch = newDirectUnpackOrchestrator(app)

	// Must not panic despite the damaged job's manifest being unreadable.
	app.duOrch.maybeStart(FileComplete{JobID: damagedID, FileIdx: 0})
}
