package queue_test

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestMoveToHistory_NoHistoryRepositoryWired pins the guard for a store
// constructed without a repository. The transition writes a history row, so
// proceeding would delete the job with nowhere for it to land.
func TestMoveToHistory_NoHistoryRepositoryWired(t *testing.T) {
	_, repo, dir := setupTestStore(t)
	store := queue.NewSQLiteStore(repo.DB(), dir, nil)

	job := newTestJob("nohistrepo000001", "no-repo")
	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := store.MoveToHistory(t.Context(), job, history.Entry{NzoID: job.ID}); err == nil {
		t.Fatal("MoveToHistory succeeded with no history repository wired")
	}
	if _, err := store.Get(t.Context(), job.ID); err != nil {
		t.Errorf("job was removed despite the failed transition: %v", err)
	}
}

// TestMoveToHistory_FailsClosedOnClosedDB pins that a broken database is
// reported rather than swallowed. persistAndCommit keeps the job in the
// queue on error, which is only safe if the error actually surfaces.
func TestMoveToHistory_FailsClosedOnClosedDB(t *testing.T) {
	store, repo, _ := setupTestStore(t)

	job := newTestJobWithManifest(t, "closeddb00000001", "closed-db", 1, 2)
	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := repo.DB().Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := store.MoveToHistory(t.Context(), job, history.Entry{
		NzoID:     job.ID,
		Name:      job.Name,
		Status:    string(constants.StatusFailed),
		Completed: time.Now(),
	})
	if err == nil {
		t.Fatal("MoveToHistory reported success against a closed database")
	}
}

// TestMoveToHistory_DuplicateEntryRollsBack pins that a history insert which
// violates the nzo_id unique index leaves the job in the queue.
//
// This is the retry-after-crash case: a finalize that committed but died
// before Queue.Remove runs again on restart. Deleting the job while the
// history insert failed would lose it entirely.
func TestMoveToHistory_DuplicateEntryRollsBack(t *testing.T) {
	store, repo, _ := setupTestStore(t)
	ctx := t.Context()

	job := newTestJobWithManifest(t, "duphistory000001", "dup-history", 1, 2)
	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	entry := history.Entry{
		NzoID:     job.ID,
		Name:      job.Name,
		Status:    string(constants.StatusFailed),
		Completed: time.Now(),
	}
	if err := repo.Add(ctx, entry); err != nil {
		t.Fatalf("seed history entry: %v", err)
	}

	if err := store.MoveToHistory(ctx, job, entry); err == nil {
		t.Fatal("MoveToHistory succeeded despite a duplicate nzo_id")
	}
	if _, err := store.Get(ctx, job.ID); err != nil {
		t.Errorf("job was deleted although its history insert failed: %v", err)
	}
	// The rolled-back transaction must not leave retained progress behind
	// either: the rows and the entry that owns them are written together.
	files, err := store.HistoryFileProgress(ctx, job.ID)
	if err != nil {
		t.Fatalf("HistoryFileProgress: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("%d retained rows survived a rolled-back transition, want 0", len(files))
	}
}
