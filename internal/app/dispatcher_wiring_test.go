package app

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/downloader"
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

type mockCancelWakeDownloader struct {
	mu        sync.Mutex
	cancelled []string
	woken     int
}

func (m *mockCancelWakeDownloader) Start(context.Context) error                      { return nil }
func (m *mockCancelWakeDownloader) Stop() error                                      { return nil }
func (m *mockCancelWakeDownloader) Completions() <-chan *downloader.ArticleResult    { return nil }
func (m *mockCancelWakeDownloader) SetSpeedLimit(int64)                              {}
func (m *mockCancelWakeDownloader) SetDispatchOptions(int, int, bool, time.Duration) {}
func (m *mockCancelWakeDownloader) UnblockServer(string) bool                        { return true }
func (m *mockCancelWakeDownloader) Pause()                                           {}
func (m *mockCancelWakeDownloader) Resume()                                          {}
func (m *mockCancelWakeDownloader) DisconnectAll()                                   {}
func (m *mockCancelWakeDownloader) CancelJob(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancelled = append(m.cancelled, id)
}
func (m *mockCancelWakeDownloader) Wake() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.woken++
}

func TestAppWorkers_Abort(t *testing.T) {
	// 1. Nil app should safely return without panic.
	wNil := &appWorkers{app: nil}
	wNil.Abort("job-nil")

	// 2. App with nil downloader and nil dispatcher should safely return.
	wEmpty := &appWorkers{app: &Application{}}
	wEmpty.Abort("job-empty")

	// 3. App with wired mock downloader and real dispatcher.
	app := newTestApplication(t)
	w := &appWorkers{app: app}

	j := job.New("job-abort", "Test Job", job.PolicyFromPP(3))
	if err := app.dispatcher.Add(j, dispatch.Header{Name: "Test Job"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	mockDL := &mockCancelWakeDownloader{}
	app.mu.Lock()
	app.downloader = mockDL
	app.mu.Unlock()

	w.Abort("job-abort")

	mockDL.mu.Lock()
	cancelledLen := len(mockDL.cancelled)
	var cancelledID string
	if cancelledLen > 0 {
		cancelledID = mockDL.cancelled[0]
	}
	wokenCount := mockDL.woken
	mockDL.mu.Unlock()

	if cancelledLen != 1 || cancelledID != "job-abort" {
		t.Errorf("CancelJob called with %v, want [job-abort]", mockDL.cancelled)
	}
	if wokenCount != 1 {
		t.Errorf("Wake called %d times, want 1", wokenCount)
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

	// Seed dispatch_jobs and job_files rows for job-1 and job-2.
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
		t.Fatalf("insert job_files job-1: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO job_files (job_id, file_index, subject, date, bytes, complete) VALUES (?, ?, ?, ?, ?, ?)`,
		"job-2", 0, "subject-2", 1700000000, 200, 0,
	); err != nil {
		t.Fatalf("insert job_files job-2: %v", err)
	}

	// Create trigger that fails whenever job_files for job-2 is updated.
	if _, err := db.ExecContext(ctx,
		`CREATE TRIGGER fail_job2 BEFORE UPDATE ON job_files WHEN NEW.job_id = 'job-2' BEGIN SELECT RAISE(ABORT, 'simulated mid-batch update failure'); END;`,
	); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Build checkpoints:
	// cp1 for job-1 has content and 1 failed article.
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
	if err := j1.SetFileFilename(0, "file1.bin"); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	cp1 := j1.Checkpoint()

	// cp2 for job-2 has content so SaveBatch executes stmtFiles on job-2 and triggers abort.
	j2 := job.New("job-2", "Job 2", job.PolicyFromPP(3))
	m2 := job.NewManifest([]job.JobFile{{
		Subject:  "file2.bin",
		Bytes:    200,
		Articles: []job.JobArticle{{ID: "art2@example.com", Bytes: 200, Number: 1}},
	}})
	if err := j2.AttachContent(m2); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	cp2 := j2.Checkpoint()

	store := &appCheckpointStore{db: db}

	// SaveBatch with [cp1, cp2] must fail mid-batch and roll back everything.
	if saveErr := store.SaveBatch(ctx, []job.Checkpoint{cp1, cp2}); saveErr == nil {
		t.Fatal("SaveBatch should have failed due to trigger")
	}

	// Verify rollback across job_files and failed_articles.
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

	// Verify committed state in job_files.
	var j1Filename string
	if err := db.QueryRowContext(ctx, `SELECT filename FROM job_files WHERE job_id = 'job-1' AND file_index = 0`).Scan(&j1Filename); err != nil {
		t.Fatalf("query j1 filename: %v", err)
	}
	if j1Filename != "file1.bin" {
		t.Errorf("committed j1 filename = %q, want %q", j1Filename, "file1.bin")
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
