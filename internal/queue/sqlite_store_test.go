package queue_test

import (
	"errors"
	"fmt"
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

	manifestPath := filepath.Join(dir, "manifests", job.ID+".json.gz")
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Errorf("manifest file should be removed on job Remove, stat err=%v", err)
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

	manifestPath := filepath.Join(dir, "manifests", job.ID+".json.gz")
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Errorf("manifest file should be removed after MoveToHistory, stat err=%v", err)
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

	if err := store.Add(ctx, fullJob); err != nil {
		t.Fatalf("Add fullJob: %v", err)
	}

	got, err := store.Get(ctx, fullJob.ID)
	if err != nil {
		t.Fatalf("Get fullJob: %v", err)
	}
	if got.Manifest() == nil || got.Manifest().NumFiles() != 2 {
		t.Fatalf("manifest not restored correctly: %v", got.Manifest())
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
	q := queue.New(queue.WithStore(store))
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
	q := queue.New(queue.WithStore(store))
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
