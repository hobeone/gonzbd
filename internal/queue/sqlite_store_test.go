package queue_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

func setupTestStore(t *testing.T) (*queue.SQLiteStore, *history.Repository, string) {
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
	return store, repo, dir
}

func newTestJob(id, name string) *queue.Job {
	return &queue.Job{
		ID:       id,
		Filename: name + ".nzb",
		Name:     name,
		Category: "default",
		Priority: constants.NormalPriority,
		Status:   constants.StatusQueued,
		PP:       3,
		Added:    time.Now().Truncate(time.Second),
		MD5:      fmt.Sprintf("md5-%s", id),
		AvgAge:   time.Now().Truncate(time.Second),
	}
}

// newTestJobWithManifest builds a job with a real manifest of nFiles files,
// each with nArticles articles — unlike newTestJob, which builds a
// manifest-less job and so cannot exercise any manifest-derived persistence
// (article_count, scalars, etc.). Constructed through queue.NewJob because
// this is the external queue_test package and cannot reach Job's unexported
// fields directly.
func newTestJobWithManifest(t *testing.T, id, name string, nFiles, nArticles int) *queue.Job {
	t.Helper()
	parsed := &nzb.NZB{
		Meta:   map[string][]string{"title": {name}},
		Groups: []string{"alt.binaries.test"},
		AvgAge: time.Unix(1700000000, 0),
	}
	for fi := range nFiles {
		f := nzb.File{
			Subject: fmt.Sprintf("%s - file %d", name, fi),
			Date:    time.Unix(1700000000, 0),
		}
		for ai := range nArticles {
			art := nzb.Article{
				ID:     fmt.Sprintf("f%da%d@test", fi, ai),
				Bytes:  100_000,
				Number: ai + 1,
			}
			f.Articles = append(f.Articles, art)
			f.Bytes += int64(art.Bytes)
		}
		parsed.Files = append(parsed.Files, f)
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{
		Filename: name + ".nzb",
		Priority: constants.NormalPriority,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	job.ID = id
	return job
}

func TestSQLiteStore_CRUD(t *testing.T) {
	store, _, dir := setupTestStore(t)
	ctx := t.Context()

	job := newTestJob("job0000000000001", "test-job-1")
	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != job.Name || got.MD5 != job.MD5 {
		t.Errorf("Get mismatch: got name=%q md5=%q, want %q, %q", got.Name, got.MD5, job.Name, job.MD5)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != job.ID {
		t.Fatalf("List expected 1 job %q, got %d jobs", job.ID, len(list))
	}

	job.Name = "updated-name"
	job.Status = constants.StatusDownloading
	if err := store.Update(ctx, job); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = store.Get(ctx, job.ID)
	if got.Name != "updated-name" || got.Status != constants.StatusDownloading {
		t.Errorf("Update failed: got %q / %q", got.Name, got.Status)
	}

	if err := store.Remove(ctx, job.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := store.Get(ctx, job.ID); !errors.Is(err, queue.ErrNotFound) {
		t.Errorf("expected ErrNotFound after Remove, got %v", err)
	}

	// store.Remove is DB-only; unlinking artifacts is DeleteJobArtifacts'
	// job, so that it can run after the job has left the queue. Write the
	// manifest first — newTestJob has no Manifest, so store.Add never created
	// one and this assertion would otherwise hold no matter which method did
	// the deleting. TestSQLiteStore_MoveToHistory pins the same split on the
	// completion path.
	manifestPath := filepath.Join(dir, "manifests", job.ID+".json.gz")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("manifest placeholder"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := store.Remove(ctx, job.ID); err != nil && !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("second Remove: %v", err)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("manifest must survive store.Remove, stat err=%v", err)
	}
	if err := store.DeleteJobArtifacts(ctx, job.ID); err != nil {
		t.Fatalf("DeleteJobArtifacts: %v", err)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Errorf("manifest should be removed by DeleteJobArtifacts, stat err=%v", err)
	}
}

func TestSQLiteStore_MoveToHistory(t *testing.T) {
	store, repo, dir := setupTestStore(t)
	ctx := t.Context()

	job := newTestJob("job0000000000002", "test-history-job")
	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	entry := history.Entry{
		NzoID:     job.ID,
		Name:      job.Name,
		Category:  job.Category,
		Status:    string(constants.StatusCompleted),
		Completed: time.Now(),
		TimeAdded: job.Added,
	}

	// Create the manifest explicitly. newTestJob carries no Manifest, so
	// store.Add never wrote one, which left the assertion at the end of this
	// test trivially satisfied: it checked that a file which had never
	// existed did not exist. Its contents are irrelevant here — this test is
	// about when the file is unlinked, not what is in it.
	manifestPath := filepath.Join(dir, "manifests", job.ID+".json.gz")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("manifest placeholder"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if err := store.MoveToHistory(ctx, job, entry); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}

	if _, err := store.Get(ctx, job.ID); !errors.Is(err, queue.ErrNotFound) {
		t.Errorf("expected job to be deleted from active store, got err=%v", err)
	}

	histEntry, err := repo.Get(ctx, job.ID)
	if err != nil || histEntry == nil {
		t.Fatalf("expected job in history repo, got err=%v", err)
	}
	if histEntry.Name != job.Name {
		t.Errorf("history name=%q, want %q", histEntry.Name, job.Name)
	}

	// MoveToHistory is now DB-only. The unlink moved to DeleteJobArtifacts so
	// it can run after the job has left the queue's byID map: Queue.Snapshot
	// clones under the read lock and hydrates from disk after releasing it,
	// so unlinking while the job is still reachable lets a snapshot catch a
	// job whose manifest has already gone.
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("manifest must survive MoveToHistory, stat err=%v", err)
	}

	if err := store.DeleteJobArtifacts(ctx, job.ID); err != nil {
		t.Fatalf("DeleteJobArtifacts: %v", err)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Errorf("manifest should be removed by DeleteJobArtifacts, stat err=%v", err)
	}
}

func TestSQLiteStore_ShiftSortKey(t *testing.T) {
	store, _, _ := setupTestStore(t)
	ctx := t.Context()

	const count = 100
	for i := range count {
		id := fmt.Sprintf("job-%04d", i)
		job := newTestJob(id, fmt.Sprintf("job-%d", i))
		if err := store.Add(ctx, job); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	list, err := store.List(ctx)
	if err != nil || len(list) != count {
		t.Fatalf("List initial: err=%v len=%d", err, len(list))
	}
	if list[0].ID != "job-0000" || list[count-1].ID != "job-0099" {
		t.Fatalf("initial order wrong: first=%s last=%s", list[0].ID, list[count-1].ID)
	}

	// Shift last item to position 0 (top of queue)
	if err := store.ShiftSortKey(ctx, "job-0099", 0); err != nil {
		t.Fatalf("ShiftSortKey to 0: %v", err)
	}

	list, _ = store.List(ctx)
	if list[0].ID != "job-0099" || list[1].ID != "job-0000" {
		t.Errorf("after shift to top: first=%s second=%s", list[0].ID, list[1].ID)
	}

	// Shift top item back to bottom
	if err := store.ShiftSortKey(ctx, "job-0099", count-1); err != nil {
		t.Fatalf("ShiftSortKey to bottom: %v", err)
	}

	list, _ = store.List(ctx)
	if list[0].ID != "job-0000" || list[count-1].ID != "job-0099" {
		t.Errorf("after shift to bottom: first=%s last=%s", list[0].ID, list[count-1].ID)
	}
}

func TestSQLiteStore_QueryPlan(t *testing.T) {
	_, repo, _ := setupTestStore(t)
	ctx := t.Context()

	checkPlan := func(query string, expectedIndex string) {
		rows, err := repo.DB().QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, "test")
		if err != nil {
			t.Fatalf("EXPLAIN QUERY PLAN %q: %v", query, err)
		}
		defer rows.Close()

		var plan strings.Builder
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err == nil {
				plan.WriteString(detail)
				plan.WriteString(" ")
			}
		}
		if !strings.Contains(plan.String(), expectedIndex) {
			t.Errorf("query %q did not use expected index %q, plan: %s", query, expectedIndex, plan.String())
		}
	}

	checkPlan("SELECT 1 FROM jobs WHERE name = ?", "idx_jobs_name")
	checkPlan("SELECT 1 FROM jobs WHERE md5 = ?", "idx_jobs_md5")
}

func TestSQLiteStore_ManifestAndFiles(t *testing.T) {
	store, _, _ := setupTestStore(t)
	ctx := t.Context()

	job := newTestJob("job0000000000003", "manifest-test")
	files := []nzb.File{
		{Subject: "file0.bin", Bytes: 100, Articles: []nzb.Article{{ID: "a0@t", Bytes: 100, Number: 1}}},
		{Subject: "file1.bin", Bytes: 200, Articles: []nzb.Article{{ID: "a1@t", Bytes: 200, Number: 1}}},
	}
	parsed := &nzb.NZB{Files: files}
	fullJob, err := queue.NewJob(parsed, queue.AddOptions{Name: "manifest-test"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	fullJob.ID = job.ID
	fullJob.Status = constants.StatusDownloading

	if err := store.Add(ctx, fullJob); err != nil {
		t.Fatalf("Add fullJob: %v", err)
	}

	got, err := store.Get(ctx, fullJob.ID)
	if err != nil {
		t.Fatalf("Get fullJob: %v", err)
	}
	if !manifestResident(got) || mustManifest(t, got).NumFiles() != 2 {
		t.Fatalf("manifest not restored correctly: %v", mustManifest(t, got))
	}
	if got.Progress() == nil {
		t.Fatal("progress not constructed")
	}
}

func TestSQLiteStore_ExistsAndPauseAndPrune(t *testing.T) {
	store, _, dir := setupTestStore(t)
	ctx := t.Context()

	if paused, err := store.IsPaused(ctx); err != nil || paused {
		t.Errorf("expected initial pause state to be false, got paused=%v err=%v", paused, err)
	}
	if err := store.SetPaused(ctx, true); err != nil {
		t.Fatalf("SetPaused(true): %v", err)
	}
	if paused, err := store.IsPaused(ctx); err != nil || !paused {
		t.Errorf("expected pause state to be true, got paused=%v err=%v", paused, err)
	}
	if err := store.SetPaused(ctx, false); err != nil {
		t.Fatalf("SetPaused(false): %v", err)
	}
	if paused, err := store.IsPaused(ctx); err != nil || paused {
		t.Errorf("expected pause state to be false after reset, got paused=%v err=%v", paused, err)
	}

	job := newTestJob("job-exists-001", "unique-job-name")
	job.MD5 = "0123456789abcdef0123456789abcdef"
	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if exists, err := store.ExistsByName(ctx, "unique-job-name"); err != nil || !exists {
		t.Errorf("expected ExistsByName to return true for existing job, got exists=%v err=%v", exists, err)
	}
	if exists, err := store.ExistsByName(ctx, "nonexistent-name"); err != nil || exists {
		t.Errorf("expected ExistsByName to return false for nonexistent job, got exists=%v err=%v", exists, err)
	}

	if exists, err := store.ExistsByMD5(ctx, "0123456789abcdef0123456789abcdef"); err != nil || !exists {
		t.Errorf("expected ExistsByMD5 to return true for existing md5, got exists=%v err=%v", exists, err)
	}
	if exists, err := store.ExistsByMD5(ctx, "11111111111111111111111111111111"); err != nil || exists {
		t.Errorf("expected ExistsByMD5 to return false for nonexistent md5, got exists=%v err=%v", exists, err)
	}

	// Test Prune removes orphaned files not matching active jobs in SQLite
	orphanedManifest := filepath.Join(dir, "manifests", "orphaned-id.json.gz")
	if err := os.MkdirAll(filepath.Dir(orphanedManifest), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(orphanedManifest, []byte("test"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := store.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(orphanedManifest); !os.IsNotExist(err) {
		t.Errorf("expected Prune to remove orphaned manifest file, stat err=%v", err)
	}
}

func TestSQLiteStore_ArticleProgressAndQuarantine(t *testing.T) {
	store, _, dir := setupTestStore(t)
	ctx := t.Context()

	// 1. Test corrupt manifest quarantine
	corruptID := "job-corrupt"
	jobCorrupt := newTestJob(corruptID, "corrupt-job")
	jobCorrupt.Status = constants.StatusDownloading
	if err := store.Add(ctx, jobCorrupt); err != nil {
		t.Fatalf("Add corrupt job: %v", err)
	}
	manifestPath := filepath.Join(dir, "manifests", corruptID+".json.gz")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("not-gzip-data"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := store.Get(ctx, corruptID)
	if err == nil {
		t.Fatal("expected Get to fail on corrupt manifest")
	}
	if _, statErr := os.Stat(manifestPath + ".corrupt"); os.IsNotExist(statErr) {
		t.Errorf("expected corrupt manifest to be renamed to .corrupt, got err=%v", statErr)
	}
}

func TestQueue_WithStoreAndSave(t *testing.T) {
	store, _, dir := setupTestStore(t)
	q := queue.New(queue.WithStore(store), queue.WithStateDir(dir))
	if q.Store() != store {
		t.Error("expected Store() to return configured store")
	}

	parsed := &nzb.NZB{
		Files: []nzb.File{
			{
				Subject:  "test_file",
				Articles: []nzb.Article{{ID: "art1", Number: 1}},
				Bytes:    100,
			},
		},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "job-save-1"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	list, err := store.List(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("List after Save: err=%v len=%d", err, len(list))
	}
	loadedQ, err := queue.Load(dir, queue.WithStore(store))
	if err != nil || loadedQ.SnapshotJob(job.ID) == nil {
		t.Fatalf("Load after Save: err=%v job missing", err)
	}
}

func TestSQLiteStore_UpdateBatch(t *testing.T) {
	store, _, _ := setupTestStore(t)
	ctx := t.Context()

	job1 := newTestJob("job-batch-001", "batch-job-1")
	job2 := newTestJob("job-batch-002", "batch-job-2")
	if err := store.Add(ctx, job1); err != nil {
		t.Fatalf("Add job1: %v", err)
	}
	if err := store.Add(ctx, job2); err != nil {
		t.Fatalf("Add job2: %v", err)
	}

	job1.Name = "batch-job-1-updated"
	job1.Status = constants.StatusDownloading
	job2.Name = "batch-job-2-updated"
	job2.Status = constants.StatusDownloading

	if err := store.UpdateBatch(ctx, []*queue.Job{job1, job2}); err != nil {
		t.Fatalf("UpdateBatch: %v", err)
	}

	got1, err := store.Get(ctx, job1.ID)
	if err != nil {
		t.Fatalf("Get job1: %v", err)
	}
	if got1.Name != "batch-job-1-updated" || got1.Status != constants.StatusDownloading {
		t.Errorf("job1 not persisted by UpdateBatch: got name=%q status=%q", got1.Name, got1.Status)
	}
	got2, err := store.Get(ctx, job2.ID)
	if err != nil {
		t.Fatalf("Get job2: %v", err)
	}
	if got2.Name != "batch-job-2-updated" || got2.Status != constants.StatusDownloading {
		t.Errorf("job2 not persisted by UpdateBatch: got name=%q status=%q", got2.Name, got2.Status)
	}
}

func TestSQLiteStore_UpdateBatchRollsBackAllOnFailure(t *testing.T) {
	store, _, _ := setupTestStore(t)
	ctx := t.Context()

	job1 := newTestJob("job-batch-rb-001", "rollback-job-1")
	job2 := newTestJob("job-batch-rb-002", "rollback-job-2")
	if err := store.Add(ctx, job1); err != nil {
		t.Fatalf("Add job1: %v", err)
	}
	if err := store.Add(ctx, job2); err != nil {
		t.Fatalf("Add job2: %v", err)
	}

	// job1 is a valid update that should succeed on its own; the batch also
	// includes a job that was never Add()ed, so its UPDATE affects zero rows
	// and updateTx returns ErrNotFound partway through the transaction. The
	// whole batch — including the otherwise-valid job1 update — must roll
	// back, proving there is no partial write.
	job1.Name = "rollback-job-1-updated"
	missingJob := newTestJob("job-batch-rb-missing", "never-added")

	err := store.UpdateBatch(ctx, []*queue.Job{job1, missingJob})
	if !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("expected UpdateBatch to fail with ErrNotFound, got %v", err)
	}

	got1, getErr := store.Get(ctx, job1.ID)
	if getErr != nil {
		t.Fatalf("Get job1: %v", getErr)
	}
	if got1.Name != "rollback-job-1" {
		t.Errorf("expected job1 update to be rolled back, got name=%q (want original %q)", got1.Name, "rollback-job-1")
	}
}

// TestSQLiteStore_AddErrorPaths exercises Add's error-handling branches that
// TestSQLiteStore_CRUD's happy path never reaches: a closed connection pool
// (BeginTx failure), a duplicate primary key (the jobs INSERT failure), a
// pre-existing conflicting job_files row (the job_files INSERT failure), and
// a resident job with a non-zero download-finished timestamp (the
// download_finished branch, which newTestJob's fresh jobs never hit).
func TestSQLiteStore_AddErrorPaths(t *testing.T) {
	ctx := t.Context()

	t.Run("begin tx fails on a closed pool", func(t *testing.T) {
		store, repo, _ := setupTestStore(t)
		if err := repo.DB().Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
		// "begin tx" rather than "begin tx add": opening the transaction
		// moved into withWriteTx, which is shared by every write path that
		// has to restart on contention.
		err := store.Add(ctx, newTestJob("closed-pool-job", "closed-pool-job"))
		if err == nil || !strings.Contains(err.Error(), "begin tx") {
			t.Fatalf("Add on closed pool = %v, want error containing %q", err, "begin tx")
		}
	})

	t.Run("duplicate job id fails the jobs insert", func(t *testing.T) {
		store, _, _ := setupTestStore(t)
		job := newTestJob("dup-job-id", "dup-job-id")
		if err := store.Add(ctx, job); err != nil {
			t.Fatalf("first Add: %v", err)
		}
		err := store.Add(ctx, newTestJob("dup-job-id", "dup-job-id-again"))
		if err == nil || !strings.Contains(err.Error(), "insert job") {
			t.Fatalf("Add with duplicate id = %v, want error containing %q", err, "insert job")
		}
	})

	t.Run("conflicting job_files row fails the job_files insert", func(t *testing.T) {
		store, repo, _ := setupTestStore(t)
		job := newTestJobWithManifest(t, "conflicting-files-job", "conflicting-files", 1, 1)
		// Add succeeds once, creating the jobs row and its one job_files
		// row. A dedicated connection then disables foreign-key enforcement
		// (SQLite refuses to toggle this pragma inside a transaction, so it
		// must run standalone) and deletes only the jobs row, leaving the
		// job_files row orphaned instead of cascading. Re-adding the same
		// job re-inserts the jobs row fine, but its insert for file 0 now
		// collides with the orphaned row on UNIQUE(job_id, file_index).
		if err := store.Add(ctx, job); err != nil {
			t.Fatalf("seed Add: %v", err)
		}
		conn, err := repo.DB().Conn(ctx)
		if err != nil {
			t.Fatalf("acquire conn: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
			t.Fatalf("disable foreign keys: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", job.ID); err != nil {
			t.Fatalf("delete jobs row directly: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
			t.Fatalf("re-enable foreign keys: %v", err)
		}

		err = store.Add(ctx, job)
		if err == nil || !strings.Contains(err.Error(), "insert job_file") {
			t.Fatalf("Add with conflicting job_files row = %v, want error containing %q", err, "insert job_file")
		}
	})

	t.Run("download-finished timestamp is persisted", func(t *testing.T) {
		store, repo, _ := setupTestStore(t)
		job := newTestJobWithManifest(t, "dl-finished-job", "dl-finished", 1, 1)
		want := time.Unix(1700001000, 0).UTC()
		job.MarkDownloadFinished(want)
		if err := store.Add(ctx, job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		// job.Status defaults to non-resident (StatusQueued), so Get won't
		// attach a manifest/progress to read the timestamp back from; assert
		// against the raw column via a direct query instead.
		var dlFinishedUnix int64
		if err := repo.DB().QueryRowContext(ctx, "SELECT download_finished FROM jobs WHERE id = ?", job.ID).Scan(&dlFinishedUnix); err != nil {
			t.Fatalf("query download_finished: %v", err)
		}
		if dlFinishedUnix != want.Unix() {
			t.Errorf("download_finished = %d, want %d", dlFinishedUnix, want.Unix())
		}
	})
}

func TestSQLiteStore_ErrorCoverage(t *testing.T) {
	store, repo, _ := setupTestStore(t)
	ctx := t.Context()

	if err := store.Update(ctx, newTestJob("nonexistent", "nonexistent")); !errors.Is(err, queue.ErrNotFound) {
		t.Errorf("expected ErrNotFound on Update nonexistent, got %v", err)
	}
	if err := store.Remove(ctx, "nonexistent"); !errors.Is(err, queue.ErrNotFound) {
		t.Errorf("expected ErrNotFound on Remove nonexistent, got %v", err)
	}
	if err := store.ShiftSortKey(ctx, "nonexistent", 0); !errors.Is(err, queue.ErrNotFound) {
		t.Errorf("expected ErrNotFound on ShiftSortKey nonexistent, got %v", err)
	}

	if err := store.MoveToHistory(ctx, newTestJob("nonexistent", "nonexistent"), history.Entry{}); !errors.Is(err, queue.ErrNotFound) {
		t.Errorf("expected ErrNotFound on MoveToHistory nonexistent, got %v", err)
	}

	ppJob := newTestJob("pp-job", "pp-job")
	ppJob.PostProc = true
	if err := store.Add(ctx, ppJob); err != nil {
		t.Fatalf("Add PostProc job: %v", err)
	}

	parsed := &nzb.NZB{
		Files: []nzb.File{
			{
				Subject:  "file.vol01+02.par2",
				Articles: []nzb.Article{{ID: "p1", Number: 1}},
				Bytes:    500,
			},
		},
	}
	par2Job, err := queue.NewJob(parsed, queue.AddOptions{Name: "par2-job"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := store.Add(ctx, par2Job); err != nil {
		t.Fatalf("Add PAR2 job: %v", err)
	}

	uninitializedStore := queue.NewSQLiteStore(repo.DB(), t.TempDir(), nil)
	if err := uninitializedStore.MoveToHistory(ctx, newTestJob("j1", "j1"), history.Entry{}); err == nil {
		t.Error("expected error when historyRepo is nil in MoveToHistory")
	}
}

func TestSQLiteStore_AbsentManifestHandling(t *testing.T) {
	store, _, dir := setupTestStore(t)
	ctx := t.Context()

	parsed := &nzb.NZB{
		Files: []nzb.File{
			{
				Subject:  "absent_file",
				Articles: []nzb.Article{{ID: "art_absent", Number: 1}},
				Bytes:    100,
			},
		},
	}
	jobAbsent, err := queue.NewJob(parsed, queue.AddOptions{Name: "absent-job"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := store.Add(ctx, jobAbsent); err != nil {
		t.Fatalf("Add absent job: %v", err)
	}

	manifestPath := filepath.Join(dir, "manifests", jobAbsent.ID+".json.gz")
	_ = os.Remove(manifestPath)

	_, err = store.Get(ctx, jobAbsent.ID)
	if err == nil {
		t.Fatal("expected Get to fail when manifest file is missing")
	}

	// Verify SQL row was removed to prevent orphaned records
	if _, err := store.Get(ctx, jobAbsent.ID); !errors.Is(err, queue.ErrNotFound) {
		t.Errorf("expected ErrNotFound after manifest missing cleanup, got %v", err)
	}
}

func TestSQLiteStore_GetFailsClosedOnFileCountQueryError(t *testing.T) {
	store, repo, _ := setupTestStore(t)
	ctx := t.Context()

	job := newTestJob("count-query-error", "count-query-error")
	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := repo.DB().ExecContext(ctx, "DROP TABLE job_files"); err != nil {
		t.Fatalf("DROP TABLE job_files: %v", err)
	}

	if _, err := store.Get(ctx, job.ID); err == nil {
		t.Fatal("expected Get to fail when the file-count query errors, got nil error")
	}
}

func TestSQLiteStore_UpdateArticleProgressRoundTrip(t *testing.T) {
	store, _, dir := setupTestStore(t)
	ctx := t.Context()

	parsed := &nzb.NZB{
		Files: []nzb.File{
			{
				Subject:  "f1",
				Articles: []nzb.Article{{ID: "art1", Number: 1}, {ID: "art2", Number: 2}},
				Bytes:    200,
			},
		},
	}
	q := queue.New(queue.WithStore(store), queue.WithStateDir(dir))
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "art-roundtrip"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Mutate progress (mark article index 1 as done)
	q.MarkArticlesDone(job.ID, []string{"art2"})

	// Update store (simulating checkpoint)
	if err := store.Update(ctx, job); err != nil {
		t.Fatalf("Update store: %v", err)
	}

	// Reload queue from store
	loadedQ, err := queue.Load(dir, queue.WithStore(store))
	if err != nil {
		t.Fatalf("queue.Load: %v", err)
	}
	loadedJob := loadedQ.SnapshotJob(job.ID)
	if loadedJob == nil {
		t.Fatal("loaded job missing from queue")
	}
	if !loadedJob.Progress().ArticleDone(1) {
		t.Error("expected article 1 to be marked done after Update checkpoint and reload")
	}
	if loadedJob.Progress().ArticleDone(0) {
		t.Error("expected article 0 to remain not done after Update checkpoint and reload")
	}
}

func TestSQLiteStore_DownloadTimestampsPersistence(t *testing.T) {
	store, _, dir := setupTestStore(t)
	ctx := t.Context()

	parsed := &nzb.NZB{
		Files: []nzb.File{
			{
				Subject:  "f1",
				Articles: []nzb.Article{{ID: "art1", Number: 1}},
				Bytes:    100,
			},
		},
	}
	q := queue.New(queue.WithStore(store), queue.WithStateDir(dir))
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "ts-persistence"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	startTime := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	if err := q.MarkJobStarted(job.ID, startTime); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	started, err := q.SetPostProcStarted(job.ID)
	if err != nil || !started {
		t.Fatalf("SetPostProcStarted: started=%v err=%v", started, err)
	}

	snap := q.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if snap.Progress().DownloadStarted().Unix() != startTime.Unix() {
		t.Errorf("DownloadStarted = %v, want %v", snap.Progress().DownloadStarted(), startTime)
	}
	if snap.Progress().DownloadFinished().IsZero() {
		t.Error("DownloadFinished should not be zero after SetPostProcStarted")
	}

	// Re-get job directly from store
	gotStore, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if gotStore.Progress() == nil {
		t.Fatal("store.Get progress is nil")
	}
	if gotStore.Progress().DownloadStarted().Unix() != startTime.Unix() {
		t.Errorf("store.Get DownloadStarted = %v, want %v", gotStore.Progress().DownloadStarted(), startTime)
	}
	if gotStore.Progress().DownloadFinished().Unix() != snap.Progress().DownloadFinished().Unix() {
		t.Errorf("store.Get DownloadFinished = %v, want %v", gotStore.Progress().DownloadFinished(), snap.Progress().DownloadFinished())
	}

	// Reload queue from disk
	loadedQ, err := queue.Load(dir, queue.WithStore(store))
	if err != nil {
		t.Fatalf("queue.Load: %v", err)
	}
	loadedJob := loadedQ.SnapshotJob(job.ID)
	if loadedJob == nil {
		t.Fatal("loaded job missing from queue after reload")
	}
	if loadedJob.Progress().DownloadStarted().Unix() != startTime.Unix() {
		t.Errorf("loaded DownloadStarted = %v, want %v", loadedJob.Progress().DownloadStarted(), startTime)
	}
	if loadedJob.Progress().DownloadFinished().Unix() != snap.Progress().DownloadFinished().Unix() {
		t.Errorf("loaded DownloadFinished = %v, want %v", loadedJob.Progress().DownloadFinished(), snap.Progress().DownloadFinished())
	}
}

func TestSQLiteStore_GetNullTextColumns(t *testing.T) {
	store, repo, _ := setupTestStore(t)
	ctx := t.Context()

	id := "null-fields-job"
	const query = `
INSERT INTO jobs
  (id, filename, name, password, url, category, priority, status, pp, script,
   time_added, md5, avg_age, groups, meta, warning, postproc, sort_key,
   download_started, download_finished)
VALUES (?, ?, ?, NULL, NULL, NULL, 0, 'FETCHING', 3, NULL, ?, 'md5-null', ?, NULL, NULL, NULL, 0, 0, 0, 0)`

	now := time.Now().Unix()
	if _, err := repo.DB().ExecContext(ctx, query, id, "null.nzb", "null-job", now, now); err != nil {
		t.Fatalf("failed to insert job with NULL text columns: %v", err)
	}

	job, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("SQLiteStore.Get failed on job with NULL text columns: %v", err)
	}
	if job == nil {
		t.Fatal("SQLiteStore.Get returned nil job")
	}
	if job.Password != "" || job.URL != "" || job.Category != "" || job.Script != "" || job.Warning != "" {
		t.Errorf("expected empty string fields for NULL text columns, got password=%q url=%q category=%q script=%q warning=%q",
			job.Password, job.URL, job.Category, job.Script, job.Warning)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("SQLiteStore.List failed: %v", err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("SQLiteStore.List returned %d jobs, expected 1 job with ID %q", len(list), id)
	}
}

// TestSQLiteStore_DeleteJobArtifactsReportsRemovalFailures covers the error
// path. A missing artifact is deliberately not an error — deletion is
// best-effort cleanup and a job may never have had one — so the only way to
// make os.Remove fail with something reportable is to put a non-empty
// directory where the file belongs.
//
// The manifest is the only artifact now. progress/<id>.json.gz was removed
// here too, but nothing ever wrote it: it was left over from a pre-SQLite
// layout and went with the payload format in #298.
func TestSQLiteStore_DeleteJobArtifactsReportsRemovalFailures(t *testing.T) {
	store, _, dir := setupTestStore(t)

	const id = "job0000000000009"
	blocker := filepath.Join(dir, "manifests", id+".json.gz")
	if err := os.MkdirAll(blocker, 0o750); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker child: %v", err)
	}

	err := store.DeleteJobArtifacts(t.Context(), id)
	if err == nil {
		t.Fatal("DeleteJobArtifacts: want error when the manifest cannot be removed, got nil")
	}
	if !strings.Contains(err.Error(), "remove manifest") {
		t.Errorf("error %q does not mention %q", err, "remove manifest")
	}
}

// TestSQLiteStore_DeleteJobArtifactsMissingIsNotAnError pins the other half:
// a job whose artifacts were never written, or were already cleaned up, must
// not make removal fail. Queue.Remove calls this unconditionally.
func TestSQLiteStore_DeleteJobArtifactsMissingIsNotAnError(t *testing.T) {
	store, _, _ := setupTestStore(t)

	if err := store.DeleteJobArtifacts(t.Context(), "job0000000000010"); err != nil {
		t.Errorf("DeleteJobArtifacts on absent artifacts = %v, want nil", err)
	}
}

// article_count is what lets progress be sized at restart without loading
// manifests. A job written by an older build has 0, which must be
// distinguishable from a real count so the caller can fall back.
func TestSQLiteStore_ArticleCountRoundTrips(t *testing.T) {
	store, _, _ := setupTestStore(t)
	ctx := t.Context()

	job := newTestJobWithManifest(t, "job0000000000020", "article-count", 2, 5)
	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	byJob, err := store.ArticleCountsByJob(ctx)
	if err != nil {
		t.Fatalf("ArticleCountsByJob: %v", err)
	}
	got := byJob[job.ID]
	want := []int{5, 5}
	if len(got) != len(want) {
		t.Fatalf("got %d files, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ArticleCount != want[i] {
			t.Errorf("file %d: article_count = %d, want %d", i, got[i].ArticleCount, want[i])
		}
	}
}

// TestSQLiteStore_ArticleCountsByJobNonContiguousIndices pins finding 6: a
// job whose job_files rows have non-contiguous file_index values (as after a
// partial delete) must have each count attributed by its actual file_index,
// not by scan-order position.
func TestSQLiteStore_ArticleCountsByJobNonContiguousIndices(t *testing.T) {
	store, repo, _ := setupTestStore(t)
	ctx := t.Context()

	job := newTestJobWithManifest(t, "job0000000000021", "gapped", 3, 2)
	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Simulate a partial delete: file_index 1 is gone, leaving 0 and 2.
	if _, err := repo.DB().ExecContext(ctx, "DELETE FROM job_files WHERE job_id = ? AND file_index = 1", job.ID); err != nil {
		t.Fatalf("delete file_index 1: %v", err)
	}
	if _, err := repo.DB().ExecContext(ctx, "UPDATE job_files SET article_count = 9 WHERE job_id = ? AND file_index = 2", job.ID); err != nil {
		t.Fatalf("set article_count for file_index 2: %v", err)
	}

	byJob, err := store.ArticleCountsByJob(ctx)
	if err != nil {
		t.Fatalf("ArticleCountsByJob: %v", err)
	}
	got := byJob[job.ID]
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 (sized to the highest file_index + 1)", len(got))
	}
	if got[0].ArticleCount != 2 {
		t.Errorf("file_index 0: article_count = %d, want 2", got[0].ArticleCount)
	}
	if got[2].ArticleCount != 9 {
		t.Errorf("file_index 2: article_count = %d, want 9 (must be attributed by "+
			"file_index, not scan-order position)", got[2].ArticleCount)
	}
}

// TestSQLiteStore_GetNonResidentScalarsFromJobFiles pins the Task 3 gap
// closure: a job whose status is non-resident (never promoted, no live
// manifest) must still read real TotalBytes/NumFiles/NumArticles from
// job_files, now that article_count makes NumArticles reconstructable too.
// Zero would be indistinguishable from a genuinely empty job, which is why
// this matters.
//
// The recovery pair used to be a documented exception here, asserted as zero,
// on the reasoning that job_files' is_par2_recovery flags only recovery
// volumes while the old figures also counted the par2 index, so aggregating
// the flag would undercount. That reasoning is retired: the figures now key on
// is_par2_recovery themselves, so the aggregate would in fact be exactly right.
//
// They still travel through dedicated jobs.recovery_bytes/recovery_files
// columns (migration 010) for a different reason. The job_files aggregate in
// Get fails soft, leaving its scalars at zero on error, and a zero recovery
// figure is not a missing reading — it is a definite claim of no repair
// capacity that two abort gates act on. The jobs row is read unconditionally.
//
// The assertion below checks "must match the manifest" rather than "must be
// zero"; it is the same property, against a source that can answer it.
func TestSQLiteStore_GetNonResidentScalarsFromJobFiles(t *testing.T) {
	store, _, _ := setupTestStore(t)
	ctx := t.Context()

	parsed := &nzb.NZB{
		Files: []nzb.File{
			{
				Subject:  "regular-file",
				Date:     time.Unix(1700000000, 0),
				Bytes:    300,
				Articles: []nzb.Article{{ID: "a1", Bytes: 100, Number: 1}, {ID: "a2", Bytes: 100, Number: 2}, {ID: "a3", Bytes: 100, Number: 3}},
			},
			{
				// A par2 recovery volume: counted by the manifest's
				// RecoveryBytes/RecoveryFiles and flagged by job_files'
				// is_par2_recovery. Since #333 both key on the same
				// classification, so the two agree by construction.
				Subject:  "file.vol01+02.par2",
				Date:     time.Unix(1700000000, 0),
				Bytes:    50,
				Articles: []nzb.Article{{ID: "a4", Bytes: 50, Number: 1}},
			},
		},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "non-resident-scalars"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if job.Status != constants.StatusQueued {
		t.Fatalf("expected a fresh job to be non-resident (StatusQueued), got %v", job.Status)
	}
	wantTotalBytes := job.TotalBytes()
	wantNumFiles := job.NumFiles()
	wantNumArticles := job.NumArticles()
	wantPar2Files := job.RecoveryFiles()
	wantPar2Bytes := job.RecoveryBytes()
	if wantPar2Files == 0 || wantPar2Bytes == 0 {
		t.Fatal("test fixture must contain at least one par2 file for this to be a meaningful check")
	}

	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if manifestResident(got) {
		t.Fatal("expected non-resident job to come back with no resident manifest")
	}
	if got.TotalBytes() != wantTotalBytes {
		t.Errorf("TotalBytes = %d, want %d", got.TotalBytes(), wantTotalBytes)
	}
	if got.NumFiles() != wantNumFiles {
		t.Errorf("NumFiles = %d, want %d", got.NumFiles(), wantNumFiles)
	}
	if got.NumArticles() != wantNumArticles {
		t.Errorf("NumArticles = %d, want %d", got.NumArticles(), wantNumArticles)
	}
	// From jobs.par2_bytes/par2_files, not from aggregating is_par2_recovery
	// — see this test's doc comment. A zero here would be the old behaviour
	// leaking back, and would render as "no par2 files" on a queued job in
	// the API.
	if got.RecoveryBytes() != wantPar2Bytes {
		t.Errorf("Par2Bytes = %d, want %d", got.RecoveryBytes(), wantPar2Bytes)
	}
	if got.RecoveryFiles() != wantPar2Files {
		t.Errorf("Par2Files = %d, want %d", got.RecoveryFiles(), wantPar2Files)
	}
}

// TestSQLiteStore_GetLogsAggregateScalarErrorInsteadOfSwallowingIt pins the
// fix for the review finding on the non-resident scalar reconstruction
// added just above: a failure in the job_files aggregate query (SUM(bytes),
// COUNT(*), SUM(article_count)) must not vanish. Unlike the file-count
// query nine lines above it in Get (which fails the whole call, per commit
// cc26bb09 — that result gates job admission), this query only feeds three
// reporting scalars for a job that is otherwise valid and fully loaded, so
// Get still succeeds and returns the job with those three at zero (its
// pre-Task-4 behavior) — but the error must be logged, not dropped.
//
// article_count is dropped from job_files (rather than dropping the whole
// table) so that the file-count query above still succeeds and the
// resident-manifest branch is unaffected — isolating the failure to
// exactly the aggregate query under test.
func TestSQLiteStore_GetLogsAggregateScalarErrorInsteadOfSwallowingIt(t *testing.T) {
	// Swap the default logger before the store is built, not after: the store
	// captures a component-scoped logger once in NewSQLiteStore rather than
	// reaching for slog.Default() at each call site, so a swap performed
	// afterwards would capture nothing and this test would report a missing
	// log entry that was in fact written.
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	store, repo, _ := setupTestStore(t)
	ctx := t.Context()

	job := newTestJobWithManifest(t, "agg-scalar-error-job", "agg-scalar-error", 1, 1)
	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := repo.DB().ExecContext(ctx, "ALTER TABLE job_files DROP COLUMN article_count"); err != nil {
		t.Fatalf("drop article_count column: %v", err)
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v, want Get to still succeed (a reporting-value failure must not fail job retrieval)", err)
	}
	if got.TotalBytes() != 0 || got.NumFiles() != 0 || got.NumArticles() != 0 {
		t.Errorf("got TotalBytes=%d NumFiles=%d NumArticles=%d, want all zero when the aggregate query itself errors",
			got.TotalBytes(), got.NumFiles(), got.NumArticles())
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "aggregate scalars") || !strings.Contains(logged, job.ID) {
		t.Errorf("expected the aggregate query error to be logged (mentioning the job id), got log output: %q", logged)
	}
}
