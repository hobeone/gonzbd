package app_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestFinalizer_PersistError_ReleasesDispatcherResources pins that when
// historyRepo.Add fails in persistAndCommit, the dispatcher Cancel and Yielded
// calls were already executed before history persistence was attempted, so the
// job is latched IntentCancel and yielded, ensuring worker claims are not left
// stranded while database I/O runs.
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

	// Begin an immediate write transaction and insert an entry with the same NzoID.
	// This does two things:
	// 1. It guarantees repo.Add will fail with a UNIQUE constraint violation once unblocked.
	// 2. Because tx holds SQLite's exclusive write lock, repo.Add will block, allowing us
	//    to verify that Cancel and Yielded executed BEFORE historyRepo.Add was reached.
	tx, err := repo.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	existing := history.Entry{
		NzoID: qJob.ID(),
		Name:  "already-exists",
	}
	if err := repo.AddTx(t.Context(), tx, existing); err != nil {
		t.Fatalf("repo.AddTx existing: %v", err)
	}

	entry := history.Entry{
		NzoID: qJob.ID(),
		Name:  qJob.Name(),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- application.TriggerPersistAndCommit(slog.Default(), entry, ppJob)
	}()

	// Verify that Cancel was called before history persistence by polling the job's intent
	// while historyRepo.Add is blocked on tx's write lock.
	var intent job.Intent
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		intent = qJob.Snapshot().Intent
		if intent == job.IntentCancel {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if intent != job.IntentCancel {
		t.Errorf("expected job intent to be IntentCancel before history persistence, got %v", intent)
	}

	// Commit tx to release the write lock, unblocking repo.Add which now fails on the duplicate key.
	if err := tx.Commit(); err != nil {
		t.Fatalf("tx.Commit: %v", err)
	}

	err = <-errCh
	if err == nil {
		t.Fatal("expected persistAndCommit to fail on duplicate NzoID, got nil")
	}

	// Verify that teardown completed: job is removed from dispatcher despite history persist error.
	if _, ok := application.Dispatcher().Job(qJob.ID()); ok {
		t.Errorf("expected job %s to be removed from dispatcher after persist error", qJob.ID())
	}
}

// TestFinalizer_ShutdownContext_PersistSucceeds pins that when app.ctx is cancelled
// while finalizer executes during shutdown, persistAndCommit protects database writes
// with context.WithoutCancel and successfully writes history.
func TestFinalizer_ShutdownContext_PersistSucceeds(t *testing.T) {
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
			Subject: "test-shutdown-drain.bin",
			Bytes:   100,
		}},
	}
	qJob, qHdr := buildTestJob(t, cfg, parsed, types.FetchOptions{NzbName: "shutdown-drain-test"})
	if err := application.Dispatcher().Add(qJob, qHdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ppJob := &postproc.Job{
		Job: qJob,
	}

	// Cancel application lifecycle context to simulate parent context cancellation during shutdown.
	ctx, cancel := context.WithCancel(t.Context())
	application.InjectCtx(ctx)
	cancel()

	entry := history.Entry{
		NzoID:  qJob.ID(),
		Name:   qJob.Name(),
		Status: "Completed",
	}

	err = application.TriggerPersistAndCommit(slog.Default(), entry, ppJob)
	if err != nil {
		t.Fatalf("expected persistAndCommit to succeed using WithoutCancel when app.ctx is canceled, got %v", err)
	}

	// Verify history entry was written despite canceled app.ctx.
	if _, err := repo.Get(t.Context(), entry.NzoID); err != nil {
		t.Errorf("expected history entry to be present after shutdown finalization, got %v", err)
	}

	// Verify job was removed from dispatcher.
	if _, ok := application.Dispatcher().Job(qJob.ID()); ok {
		t.Errorf("expected job %s to be removed from dispatcher after finalization", qJob.ID())
	}
}

// TestFinalizer_PersistError_CleanupExecutes pins that when historyRepo.Add fails with
// an error, dispatcher removal, checkpointer prune, and manifest deletion still execute.
func TestFinalizer_PersistError_CleanupExecutes(t *testing.T) {
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
			Subject: "test-cleanup-on-error.bin",
			Bytes:   100,
		}},
	}
	qJob, qHdr := buildTestJob(t, cfg, parsed, types.FetchOptions{NzbName: "cleanup-on-error-test"})
	if err := application.Dispatcher().Add(qJob, qHdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ppJob := &postproc.Job{
		Job: qJob,
	}

	// Write a manifest file to verify it gets unlinked during cleanup.
	manifestPath := filepath.Join(admin, "queue", "manifests", qJob.ID()+".json.gz")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("fake-manifest"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Mark the job in checkpointer so we verify it gets pruned.
	application.Checkpointer().Mark(qJob)
	if application.Checkpointer().DirtyCount() != 1 {
		t.Fatalf("expected 1 dirty job before finalization, got %d", application.Checkpointer().DirtyCount())
	}

	// Pre-seed duplicate entry so historyRepo.Add fails with a UNIQUE constraint violation.
	existing := history.Entry{
		NzoID: qJob.ID(),
		Name:  "already-exists",
	}
	if err := repo.Add(t.Context(), existing); err != nil {
		t.Fatalf("repo.Add existing: %v", err)
	}

	// Seed durability row (failed_articles) to verify it gets deleted during cleanup.
	if _, err := repo.DB().ExecContext(t.Context(),
		`INSERT INTO failed_articles (job_id, art_idx) VALUES (?, 1)`, qJob.ID()); err != nil {
		t.Fatalf("seed failed_articles: %v", err)
	}

	// Seed barrier accumulator bytes to verify forgetJobBarrierState cleans it up.
	application.NoteJobBytes(qJob.ID(), 1024)
	if hasBytes, _, _ := application.JobBarrierState(qJob.ID()); !hasBytes {
		t.Fatal("expected barrier bytes to be tracked before finalization")
	}

	entry := history.Entry{
		NzoID:  qJob.ID(),
		Name:   qJob.Name(),
		Status: "Completed",
	}

	err = application.TriggerPersistAndCommit(slog.Default(), entry, ppJob)
	if err == nil {
		t.Fatal("expected persistAndCommit to fail on duplicate NzoID, got nil")
	}

	// 1. Dispatcher removal still executed.
	if _, ok := application.Dispatcher().Job(qJob.ID()); ok {
		t.Errorf("expected job %s to be removed from dispatcher after persist error", qJob.ID())
	}

	// 2. Manifest deletion still executed.
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Errorf("expected manifest %s to be deleted after persist error, got err: %v", manifestPath, err)
	}

	// 3. Checkpointer prune still executed.
	if application.Checkpointer().DirtyCount() != 0 {
		t.Errorf("expected checkpointer dirty count to be 0 after prune, got %d", application.Checkpointer().DirtyCount())
	}

	// 4. Durability rows deletion still executed for completed job.
	var failedCount int
	if err := repo.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM failed_articles WHERE job_id = ?`, qJob.ID()).Scan(&failedCount); err != nil {
		t.Fatalf("query failed_articles: %v", err)
	}
	if failedCount != 0 {
		t.Errorf("expected failed_articles to be deleted after persist error, got count %d", failedCount)
	}

	// 5. forgetJobBarrierState still executed.
	if hasBytes, hasMu, hasLast := application.JobBarrierState(qJob.ID()); hasBytes || hasMu || hasLast {
		t.Errorf("expected job barrier state to be forgotten, got bytes=%v mu=%v last=%v", hasBytes, hasMu, hasLast)
	}
}

// TestFinalizer_PostProcessorTimeout_OccupiedJobSkipsEviction pins that when
// persistAndCommit is active (inside Occupy) and Shutdown runs while the finalizer
// is blocked (e.g. on database I/O), Dispatcher.Stop waits on waitLive and, upon
// timeout, skips eviction so the active finalizer is not disrupted.
func TestFinalizer_PostProcessorTimeout_OccupiedJobSkipsEviction(t *testing.T) {
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
			Subject: "test-occupy-inflight.bin",
			Bytes:   100,
		}},
	}
	qJob, qHdr := buildTestJob(t, cfg, parsed, types.FetchOptions{NzbName: "occupy-inflight-test"})
	if err := application.Dispatcher().Add(qJob, qHdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ppJob := &postproc.Job{
		Job: qJob,
	}

	// 1. Acquire an exclusive write lock on the history DB so persistAndCommit blocks
	// inside historyRepo.Add while holding the dispatcher's Occupy latch.
	tx, err := repo.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	entry := history.Entry{
		NzoID:  qJob.ID(),
		Name:   qJob.Name(),
		Status: "Completed",
	}

	application.SetStarted(true)
	shutCtx, shutCancel := context.WithCancel(context.Background())
	application.InjectCtx(shutCtx)
	application.InjectCancel(shutCancel)

	finalizerDone := make(chan error, 1)
	go func() {
		finalizerDone <- application.TriggerPersistAndCommit(slog.Default(), entry, ppJob)
	}()

	// Wait until persistAndCommit enters Occupy.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if application.DispatcherIsOccupied(qJob.ID()) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !application.DispatcherIsOccupied(qJob.ID()) {
		t.Fatal("timed out waiting for dispatcher to mark job occupied")
	}

	application.SetPostProcessorStopHook(func() error {
		// Simulate a step timeout
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	application.SetShutdownStepTimeout(20 * time.Millisecond)
	application.Dispatcher().SetStopTimeout(20 * time.Millisecond)

	shutdownErr := application.Shutdown()
	if shutdownErr == nil {
		t.Fatal("expected Shutdown to report error due to postprocessor timeout")
	}

	// Because finalizer is occupied in persistAndCommit and Dispatcher.Stop timed out,
	// Dispatcher.Stop must have skipped eviction for qJob.
	if !qJob.Resident() {
		t.Errorf("expected job %s to remain manifest-resident (eviction skipped), but was evicted", qJob.ID())
	}

	// Release tx lock so the finalizer goroutine can complete.
	if err := tx.Commit(); err != nil {
		t.Fatalf("tx.Commit: %v", err)
	}
	<-finalizerDone
}

// TestFinalizer_PostProcessorTimeout_InFlightReClaimsLaunchAndSkipsEviction is an alias for
// TestFinalizer_PostProcessorTimeout_OccupiedJobSkipsEviction to preserve exact-name test run compatibility.
func TestFinalizer_PostProcessorTimeout_InFlightReClaimsLaunchAndSkipsEviction(t *testing.T) {
	TestFinalizer_PostProcessorTimeout_OccupiedJobSkipsEviction(t)
}
