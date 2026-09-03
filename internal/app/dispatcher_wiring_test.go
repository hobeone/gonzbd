package app

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestApplicationConstructsAWiredDispatcher pins that app.New produces a
// dispatcher with both ports satisfied. dispatch.New panics on a nil Residency
// or Runner, so this test failing to panic IS the assertion.
func TestApplicationConstructsAWiredDispatcher(t *testing.T) {
	app := newTestApplication(t)
	if app.Dispatcher() == nil {
		t.Fatal("app.New must construct a Dispatcher")
	}
	if app.Config() == nil {
		t.Fatal("app.Config() must not be nil")
	}

	w := &appWorkers{app: app}
	w.Abort("test-job")

	appNilDisp := &Application{}
	if _, ok := appNilDisp.lookupJob("test-job"); ok {
		t.Fatal("lookupJob must return false when dispatcher is nil")
	}
}

func TestAppCheckpointStore_SaveBatch_TransactionalRollback(t *testing.T) {
	ctx := t.Context()
	hdb, err := history.Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	defer hdb.Close()
	repo := history.NewRepository(hdb)
	db := repo.DB()

	// Seed dispatch_jobs and job_files rows.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO dispatch_jobs (id, sort_key, name, state) VALUES (?, ?, ?, ?)`,
		"job-1", 1, "Job 1", 10,
	); err != nil {
		t.Fatalf("insert job-1: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO dispatch_jobs (id, sort_key, name, state) VALUES (?, ?, ?, ?)`,
		"job-2", 2, "Job 2", 10,
	); err != nil {
		t.Fatalf("insert job-2: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO job_files (job_id, file_index, subject, date, bytes, complete) VALUES (?, ?, ?, ?, ?, ?)`,
		"job-1", 0, "subject-1", 1700000000, 100, 0,
	); err != nil {
		t.Fatalf("insert job_files: %v", err)
	}

	// Create trigger that fails whenever job-2 is updated.
	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER fail_job2 BEFORE UPDATE ON dispatch_jobs WHEN NEW.id = 'job-2' BEGIN SELECT RAISE(ABORT, 'simulated mid-batch update failure'); END;`,
	); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Build checkpoints:
	// cp1 for job-1 has content, 1 failed article, timestamps, par2 release reason.
	j1 := job.New("job-1", "Job 1", job.PolicyFromPP(3))
	m1 := job.NewManifest([]job.JobFile{{
		Subject:  "file1.bin",
		Bytes:    100,
		Articles: []job.JobArticle{{ID: "art1@example.com", Bytes: 100, Number: 1}},
	}})
	if err := j1.AttachContent(m1); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := j1.MarkArticleFailed(0); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	start := time.Unix(1700000010, 0).UTC()
	finish := time.Unix(1700000090, 0).UTC()
	if err := j1.MarkJobStarted(start); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	if err := j1.MarkDownloadFinished(finish); err != nil {
		t.Fatalf("MarkDownloadFinished: %v", err)
	}
	j1.SetPar2ReleaseReason("damage detected")
	cp1 := j1.Checkpoint()
	cp1.State.State = 20

	// cp2 for job-2 is a simple checkpoint whose update will trigger abort.
	j2 := job.New("job-2", "Job 2", job.PolicyFromPP(3))
	cp2 := j2.Checkpoint()
	cp2.State.State = 20

	store := &appCheckpointStore{db: db}

	// SaveBatch with [cp1, cp2] must fail mid-batch and roll back everything.
	if saveErr := store.SaveBatch(ctx, []job.Checkpoint{cp1, cp2}); saveErr == nil {
		t.Fatal("SaveBatch should have failed due to trigger")
	}

	// Verify rollback across dispatch_jobs, job_files, and failed_articles.
	var j1State int
	if err := db.QueryRowContext(ctx, `SELECT state FROM dispatch_jobs WHERE id = 'job-1'`).Scan(&j1State); err != nil {
		t.Fatalf("query j1 state: %v", err)
	}
	if j1State != 10 {
		t.Errorf("job-1 state = %d, want 10 (rolled back)", j1State)
	}

	var j1Complete int
	if err := db.QueryRowContext(ctx, `SELECT complete FROM job_files WHERE job_id = 'job-1' AND file_index = 0`).Scan(&j1Complete); err != nil {
		t.Fatalf("query j1 complete: %v", err)
	}
	if j1Complete != 0 {
		t.Errorf("job-1 complete = %d, want 0 (rolled back)", j1Complete)
	}

	var failedCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM failed_articles WHERE job_id = 'job-1'`).Scan(&failedCount); err != nil {
		t.Fatalf("query failed_articles count: %v", err)
	}
	if failedCount != 0 {
		t.Errorf("failed_articles count = %d, want 0 (rolled back)", failedCount)
	}

	// Now drop the trigger and confirm that a successful SaveBatch commits all changes.
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_job2`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	if err := store.SaveBatch(ctx, []job.Checkpoint{cp1, cp2}); err != nil {
		t.Fatalf("SaveBatch without trigger failed: %v", err)
	}

	// Verify committed state in dispatch_jobs.
	var dlStarted, dlFinished int64
	var par2Reason string
	if err := db.QueryRowContext(ctx,
		`SELECT state, download_started, download_finished, par2_release_reason FROM dispatch_jobs WHERE id = 'job-1'`,
	).Scan(&j1State, &dlStarted, &dlFinished, &par2Reason); err != nil {
		t.Fatalf("query j1 committed: %v", err)
	}
	if j1State != 20 {
		t.Errorf("committed j1 state = %d, want 20", j1State)
	}
	if dlStarted != start.Unix() {
		t.Errorf("committed j1 download_started = %d, want %d", dlStarted, start.Unix())
	}
	if dlFinished != finish.Unix() {
		t.Errorf("committed j1 download_finished = %d, want %d", dlFinished, finish.Unix())
	}
	if par2Reason != "damage detected" {
		t.Errorf("committed j1 par2Reason = %q, want %q", par2Reason, "damage detected")
	}

	// Verify committed state in failed_articles.
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM failed_articles WHERE job_id = 'job-1' AND art_idx = 0`).Scan(&failedCount); err != nil {
		t.Fatalf("query failed_articles: %v", err)
	}
	if failedCount != 1 {
		t.Errorf("failed_articles count = %d, want 1", failedCount)
	}
}

func TestAppCheckpointStore_SaveBatch_NilOrEmpty(t *testing.T) {
	sNil := &appCheckpointStore{db: nil}
	if err := sNil.SaveBatch(t.Context(), []job.Checkpoint{{ID: "x"}}); err != nil {
		t.Errorf("nil db should return nil, got %v", err)
	}
	hdb, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	defer hdb.Close()
	repo := history.NewRepository(hdb)
	s := &appCheckpointStore{db: repo.DB()}
	if err := s.SaveBatch(t.Context(), nil); err != nil {
		t.Errorf("empty slice should return nil, got %v", err)
	}
}
