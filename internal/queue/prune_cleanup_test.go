package queue_test

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// TestPruneReleasesRetainedProgress pins that retention removes a failed
// job's retained per-file progress along with its history entry.
//
// Repository.Prune used to issue its own DELETE FROM history, bypassing
// Delete and so leaving history_job_files rows behind with no entry to own
// them (#303). Nothing noticed, because Prune had no production caller —
// wiring retention up without fixing it would have turned a latent leak into
// a real one on the first sweep.
func TestPruneReleasesRetainedProgress(t *testing.T) {
	store, repo, _ := setupTestStore(t)
	ctx := t.Context()

	job := newTestJobWithManifest(t, "prunecleanup0001", "prune-cleanup", 2, 3)
	if err := store.Add(ctx, job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := store.MoveToHistory(ctx, job, history.Entry{
		NzoID:     job.ID,
		Name:      job.Name,
		Status:    string(constants.StatusFailed),
		Completed: time.Now().AddDate(0, 0, -90),
	}); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}

	// Precondition: the rows exist, so their absence below means something.
	files, err := store.HistoryFileProgress(ctx, job.ID)
	if err != nil {
		t.Fatalf("HistoryFileProgress: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no retained progress was written; the test would prove nothing")
	}

	expired, err := repo.ExpiredEntries(ctx, 0, 30)
	if err != nil {
		t.Fatalf("ExpiredEntries: %v", err)
	}
	if len(expired) != 1 || expired[0].NzoID != job.ID {
		t.Fatalf("expired = %v, want just %s", expired, job.ID)
	}
	if _, err := repo.Delete(ctx, expired[0].NzoID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	files, err = store.HistoryFileProgress(ctx, job.ID)
	if err != nil {
		t.Fatalf("HistoryFileProgress after prune: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("%d retained rows outlived the entry that owned them", len(files))
	}
}

// TestExpiredEntriesRespectsStatusSplit pins that the two thresholds apply to
// the statuses they name, so a short window on successes cannot sweep away a
// failed job an operator is keeping in order to retry it.
func TestExpiredEntriesRespectsStatusSplit(t *testing.T) {
	_, repo, _ := setupTestStore(t)
	ctx := t.Context()

	old := time.Now().AddDate(0, 0, -60)
	for _, e := range []history.Entry{
		{NzoID: "splitdone0000001", Name: "done", Status: string(constants.StatusCompleted), Completed: old},
		{NzoID: "splitfail0000001", Name: "fail", Status: string(constants.StatusFailed), Completed: old},
	} {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add %s: %v", e.NzoID, err)
		}
	}

	// Successes expire after 30 days; failures are kept forever.
	expired, err := repo.ExpiredEntries(ctx, 30, 0)
	if err != nil {
		t.Fatalf("ExpiredEntries: %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("expired %d entries, want 1", len(expired))
	}
	if expired[0].NzoID != "splitdone0000001" {
		t.Errorf("expired %s, want the completed entry; a retryable failure was swept",
			expired[0].NzoID)
	}
}
