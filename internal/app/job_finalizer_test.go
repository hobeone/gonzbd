package app_test

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestFinalizer_PersistError_ReleasesDispatcherResources pins that when
// historyRepo.Add fails in persistAndCommit, the dispatcher Cancel and Yielded
// calls were already executed, so the job is latched IntentCancel and yielded,
// ensuring worker claims are not left stranded and Dispatcher.Remove can
// complete without hanging indefinitely on waitLaunched.
func TestFinalizer_PersistError_ReleasesDispatcherResources(t *testing.T) {
	t.Parallel()
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)

	db, err := history.Open(t.Context(), filepath.Join(admin, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)

	application, err := app.New(cfg, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject: "test-finalizer.bin",
			Bytes:   100,
		}},
	}
	qJob, qHdr := buildTestJob(t, cfg, parsed, types.FetchOptions{NzbName: "finalizer-test"})
	if err := application.Dispatcher().Add(qJob, qHdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ppJob := &postproc.Job{
		Job: qJob,
	}

	// Insert an existing entry with the same NzoID into historyRepo to trigger
	// a SQLite UNIQUE constraint violation when persistAndCommit calls Add.
	existing := history.Entry{
		NzoID: qJob.ID(),
		Name:  "already-exists",
	}
	if err := repo.Add(t.Context(), existing); err != nil {
		t.Fatalf("repo.Add existing: %v", err)
	}

	entry := history.Entry{
		NzoID: qJob.ID(),
		Name:  qJob.Name(),
	}

	err = application.TriggerPersistAndCommit(slog.Default(), entry, ppJob)
	if err == nil {
		t.Fatal("expected persistAndCommit to fail on duplicate NzoID, got nil")
	}

	// Verify that Cancel was called by checking the job's intent is IntentCancel
	dj, ok := application.Dispatcher().Job(qJob.ID())
	if !ok {
		t.Fatalf("expected job %s to remain in dispatcher after failed persist", qJob.ID())
	}
	if dj.Snapshot().Intent != job.IntentCancel {
		t.Errorf("expected job intent to be IntentCancel after persist failure, got %v", dj.Snapshot().Intent)
	}

	// Verify that dispatcher.Remove can succeed without hanging
	// (worker claims cleared via Yielded/Cancel)
	removeCtx := t.Context()
	if err := application.Dispatcher().Remove(removeCtx, qJob.ID()); err != nil {
		t.Errorf("expected Dispatcher.Remove to succeed after persist error, got %v", err)
	}
}
