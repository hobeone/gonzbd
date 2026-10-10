package history

import (
	"testing"
	"time"
)

// retainedEntry is a minimal Failed entry: the status the retained progress
// existed for.
func retainedEntry(nzoID string) Entry {
	return Entry{
		NzoID:     nzoID,
		Name:      "retained-" + nzoID,
		NzbName:   "retained.nzb",
		Status:    "Failed",
		Completed: time.Unix(1_700_000_000, 0),
	}
}

// TestAdd_StoresJustTheEntry: Add writes the entry and no per-file progress.
// A retry recovers what it can from the job_files and written_articles rows
// instead.
func TestAdd_StoresJustTheEntry(t *testing.T) {
	_, repo := openTestDB(t)

	if err := repo.Add(t.Context(), retainedEntry("job-bare")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := repo.Get(t.Context(), "job-bare"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	rows, err := repo.RetainedFiles(t.Context(), "job-bare")
	if err != nil {
		t.Fatalf("RetainedFiles: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d retained rows for an entry, want 0", len(rows))
	}
}

// TestDelete_TakesRetainedProgressWithTheEntry pins that this package removes
// an entry and any history_job_files rows under its id in one transaction.
// Nothing writes those rows any more, so the fixture inserts one directly.
func TestDelete_TakesRetainedProgressWithTheEntry(t *testing.T) {
	_, repo := openTestDB(t)

	if err := repo.Add(t.Context(), retainedEntry("job-delete")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := repo.DB().ExecContext(t.Context(), `
INSERT INTO history_job_files
  (job_id, file_index, complete, fetch_policy, filename, assembled_crc32, article_count)
VALUES ('job-delete', 0, 1, 0, 'a.bin', 0, 2)`); err != nil {
		t.Fatalf("seed history_job_files: %v", err)
	}
	if _, err := repo.Delete(t.Context(), "job-delete"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	rows, err := repo.RetainedFiles(t.Context(), "job-delete")
	if err != nil {
		t.Fatalf("RetainedFiles: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d retained rows outlived their entry, want 0", len(rows))
	}
}
