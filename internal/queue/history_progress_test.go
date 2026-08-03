package queue_test

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/queue"
)

// moveToHistoryWithStatus adds a job with a manifest, marks one article of
// file 0 done so the retained bitmap is non-trivial, and moves it to history
// under the given status.
func moveToHistoryWithStatus(t *testing.T, status constants.Status) (*queue.SQLiteStore, *history.Repository, *queue.Job) {
	t.Helper()
	store, repo, _ := setupTestStore(t)

	job := newTestJobWithManifest(t, "histprog00000001", "hist-progress", 2, 3)
	if err := store.Add(t.Context(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	entry := history.Entry{
		NzoID:     job.ID,
		Name:      job.Name,
		Status:    string(status),
		Completed: time.Now(),
		TimeAdded: job.Added,
	}
	if err := store.MoveToHistory(t.Context(), job, entry); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}
	return store, repo, job
}

// TestMoveToHistory_RetainsProgressForFailedJob pins that a failed job's
// per-file progress survives the queue→history transition. Without it a
// retry cannot tell which articles already succeeded and refetches the whole
// job — including the case where every article is present and only
// post-processing failed.
func TestMoveToHistory_RetainsProgressForFailedJob(t *testing.T) {
	store, _, job := moveToHistoryWithStatus(t, constants.StatusFailed)

	files, err := store.HistoryFileProgress(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("HistoryFileProgress: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("retained %d files, want 2", len(files))
	}
	for i, f := range files {
		if f.FileIndex != i {
			t.Errorf("files[%d].FileIndex = %d, want %d", i, f.FileIndex, i)
		}
		if f.ArticleCount != 3 {
			t.Errorf("files[%d].ArticleCount = %d, want 3", i, f.ArticleCount)
		}
	}
}

// TestMoveToHistory_RetainsNothingForCompletedJob pins that a successful job
// writes no retained rows. Retaining for every job is what made the format
// this replaces grow without bound, and a completed job has nothing to retry.
func TestMoveToHistory_RetainsNothingForCompletedJob(t *testing.T) {
	store, _, job := moveToHistoryWithStatus(t, constants.StatusCompleted)

	files, err := store.HistoryFileProgress(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("HistoryFileProgress: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("retained %d files for a completed job, want 0", len(files))
	}
}

// TestHistoryDelete_RemovesRetainedProgress pins that the retained rows go
// when the history entry that owns them is deleted. They have no foreign key
// to cascade from, so this is the only thing keeping them from accumulating
// exactly like the payload format they replace — and it lives inside
// Repository.Delete rather than at its call sites so every deletion path,
// present and future, gets it without having to remember.
func TestHistoryDelete_RemovesRetainedProgress(t *testing.T) {
	store, repo, job := moveToHistoryWithStatus(t, constants.StatusFailed)

	if _, err := repo.Delete(t.Context(), job.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	files, err := store.HistoryFileProgress(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("HistoryFileProgress: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("%d rows survived deletion, want 0", len(files))
	}
}
