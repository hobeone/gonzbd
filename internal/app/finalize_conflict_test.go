package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestFinalize_RemovesJobFromQueueWhenHistoryWriteFails pins that when the
// history write fails during finalization, the job is still removed from the
// dispatcher and queue teardown completes so that the job does not leak
// permanently into the active registry.
//
// A duplicate nzo_id is the realistic way to make historyRepo.Add fail for real
// rather than through an injected fake; history.nzo_id carries a UNIQUE index,
// and a finalize re-run after a crash between the history commit and Dispatcher.Remove
// is how it happens in practice. In that scenario, the history entry already
// exists and is legitimate. Removing the job from the active queue prevents
// zombie leaks while preserving the existing history entry intact.
func TestFinalize_RemovesJobFromQueueWhenHistoryWriteFails(t *testing.T) {
	adminDir := t.TempDir()
	cfg := testConfigInternal(t, adminDir)

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	application, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })
	application.PauseDownloads()
	application.Dispatcher().Pause()

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "file.bin",
		Bytes:    1024,
		Articles: []nzb.Article{{ID: "fin1@t", Bytes: 1024, Number: 1}},
	}}}
	job, hdr, err := BuildIngestJob(application.config, parsed, "finalize.nzb", types.FetchOptions{JobID: "finalizeconflict"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(job, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Occupy the nzo_id so the history insert inside MoveToHistory violates
	// its unique index.
	if err := repo.Add(ctx, history.Entry{
		NzoID:     job.ID(),
		Name:      "already-there",
		Status:    string(constants.StatusCompleted),
		Completed: time.Now(),
	}); err != nil {
		t.Fatalf("seed conflicting entry: %v", err)
	}

	newJobFinalizer(application).finalize(&postproc.Job{
		Job:         job,
		FinalDir:    t.TempDir(),
		DownloadDir: t.TempDir(),
	})

	if _, ok := application.Dispatcher().Job(job.ID()); ok {
		t.Error("job remained in the queue although its history write failed; expected teardown to complete")
	}
	entry, err := repo.Get(ctx, job.ID())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry.Name != "already-there" {
		t.Errorf("the seeded entry was overwritten: name = %q", entry.Name)
	}
}

// TestFinalize_PreservesDurabilityWhenConflictingEntryIsFailed pins that when
// finalization collides with a pre-existing Failed history entry (e.g. following
// a crash between history commit and Dispatcher.Remove), teardown removes the
// job from the dispatcher to prevent leaks, but preserves its durability rows
// so that a subsequent history retry does not have its partial state destroyed.
func TestFinalize_PreservesDurabilityWhenConflictingEntryIsFailed(t *testing.T) {
	adminDir := t.TempDir()
	cfg := testConfigInternal(t, adminDir)

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	application, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })
	application.PauseDownloads()
	application.Dispatcher().Pause()

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "file.bin",
		Bytes:    1024,
		Articles: []nzb.Article{{ID: "fin2@t", Bytes: 1024, Number: 1}},
	}}}
	job, hdr, err := BuildIngestJob(application.config, parsed, "finalize-fail.nzb", types.FetchOptions{JobID: "failconflict"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(job, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// 1. Seed a pre-existing history entry with StatusFailed.
	if err := repo.Add(ctx, history.Entry{
		NzoID:     job.ID(),
		Name:      "existing-failed-job",
		Status:    string(constants.StatusFailed),
		Completed: time.Now(),
	}); err != nil {
		t.Fatalf("seed conflicting failed entry: %v", err)
	}

	// 2. Seed durability rows (job_files, durable_runs, failed_articles).
	seedDurability(t, application, job.ID())
	if _, err := application.historyRepo.DB().ExecContext(ctx, `
INSERT INTO job_files (job_id, file_index, subject, date, bytes, complete, fetch_policy, filename, assembled_crc32, article_count)
VALUES (?, 0, "file.bin", 0, 1024, 0, 0, "file.bin", 0, 1)`, job.ID()); err != nil {
		t.Fatalf("seed job_files: %v", err)
	}

	nf, ne := durabilityRowCounts(t, application, job.ID())
	if nf != 1 || ne != 1 {
		t.Fatalf("fixture seeded %d runs and %d failed rows, want 1 and 1", nf, ne)
	}

	// 3. Finalize a job with that ID (simulating a re-run of finalize where status was Completed).
	newJobFinalizer(application).finalize(&postproc.Job{
		Job:         job,
		FinalDir:    t.TempDir(),
		DownloadDir: t.TempDir(),
	})

	// 4. Verify the job is removed from dispatcher.
	if _, ok := application.Dispatcher().Job(job.ID()); ok {
		t.Error("job remained in dispatcher; expected teardown to remove it")
	}

	// 5. Verify durability rows are NOT deleted.
	nfAfter, neAfter := durabilityRowCounts(t, application, job.ID())
	if nfAfter != 1 || neAfter != 1 {
		t.Errorf("durability rows were deleted on conflict with failed entry: runs=%d, failed=%d, want 1 and 1",
			nfAfter, neAfter)
	}
	var jobFilesCount int
	if err := application.historyRepo.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_files WHERE job_id = ?`, job.ID()).Scan(&jobFilesCount); err != nil {
		t.Fatalf("count job_files: %v", err)
	}
	if jobFilesCount != 1 {
		t.Errorf("job_files rows were deleted on conflict with failed entry: count=%d, want 1", jobFilesCount)
	}
}

// TestFinalize_PreservesDurabilityWhenHistoryLookupReturnsError pins that when
// history persistence fails and historyRepo.Get returns an unexpected error
// (e.g. SQLite error or context canceled, anything other than history.ErrNotFound),
// durability rows are preserved on doubt rather than deleted.
func TestFinalize_PreservesDurabilityWhenHistoryLookupReturnsError(t *testing.T) {
	adminDir := t.TempDir()
	cfg := testConfigInternal(t, adminDir)

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	application, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })
	application.PauseDownloads()
	application.Dispatcher().Pause()

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "file.bin",
		Bytes:    1024,
		Articles: []nzb.Article{{ID: "fin3@t", Bytes: 1024, Number: 1}},
	}}}
	job, hdr, err := BuildIngestJob(application.config, parsed, "finalize-dberr.nzb", types.FetchOptions{JobID: "dberrconflict"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(job, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// 1. Seed durability rows (job_files, durable_runs, failed_articles).
	seedDurability(t, application, job.ID())
	if _, err := application.historyRepo.DB().ExecContext(ctx, `
INSERT INTO job_files (job_id, file_index, subject, date, bytes, complete, fetch_policy, filename, assembled_crc32, article_count)
VALUES (?, 0, "file.bin", 0, 1024, 0, 0, "file.bin", 0, 1)`, job.ID()); err != nil {
		t.Fatalf("seed job_files: %v", err)
	}

	nf, ne := durabilityRowCounts(t, application, job.ID())
	if nf != 1 || ne != 1 {
		t.Fatalf("fixture seeded %d runs and %d failed rows, want 1 and 1", nf, ne)
	}

	// 2. Corrupt the history schema so historyRepo.Add and historyRepo.Get both fail
	// with a table missing / SQLite query error (not ErrNotFound).
	if _, err := application.historyRepo.DB().ExecContext(ctx, `DROP TABLE history`); err != nil {
		t.Fatalf("drop history table: %v", err)
	}

	// 3. Finalize the job with non-failed status. History write will fail because
	// history table is gone. Then history lookup will fail with an error
	// (sqlite table not found, which is NOT history.ErrNotFound).
	newJobFinalizer(application).finalize(&postproc.Job{
		Job:         job,
		FinalDir:    t.TempDir(),
		DownloadDir: t.TempDir(),
	})

	// 4. Verify durability rows are NOT deleted (preserved on doubt).
	nfAfter, neAfter := durabilityRowCounts(t, application, job.ID())
	if nfAfter != 1 || neAfter != 1 {
		t.Errorf("durability rows were deleted when history lookup failed with error: runs=%d, failed=%d, want 1 and 1",
			nfAfter, neAfter)
	}
	var jobFilesCount int
	if err := application.historyRepo.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_files WHERE job_id = ?`, job.ID()).Scan(&jobFilesCount); err != nil {
		t.Fatalf("count job_files: %v", err)
	}
	if jobFilesCount != 1 {
		t.Errorf("job_files rows were deleted when history lookup failed with error: count=%d, want 1", jobFilesCount)
	}
}

// testConfigInternal builds a minimal config for package-internal tests.
func testConfigInternal(t *testing.T, adminDir string) *config.Config {
	t.Helper()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.General.AdminDir = adminDir
	cfg.General.DownloadDir = t.TempDir()
	cfg.General.CompleteDir = t.TempDir()
	cfg.Servers = []config.ServerConfig{{
		Name: "mock", Host: "127.0.0.1", Port: 1119, Enable: false, Connections: 1,
	}}
	return cfg
}
