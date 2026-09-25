package history

import (
	"testing"
	"time"
)

// retainedEntry is a minimal Failed entry: the status the retained progress
// exists for.
func retainedEntry(nzoID string) Entry {
	return Entry{
		NzoID:     nzoID,
		Name:      "retained-" + nzoID,
		NzbName:   "retained.nzb",
		Status:    "Failed",
		Completed: time.Unix(1_700_000_000, 0),
	}
}

// TestAdd_StoresRetainedProgressWithItsEntry pins the round trip: what Add
// writes is what RetainedFiles reads back, every field of it.
//
// FetchPolicy is asserted alongside the rest because a read that quietly drops
// one column is exactly the regression a round trip exists to catch.
func TestAdd_StoresRetainedProgressWithItsEntry(t *testing.T) {
	_, repo := openTestDB(t)

	want := []FileProgress{
		{FileIndex: 0, Complete: true, FetchPolicy: 2, Filename: "a.bin", AssembledCRC32: 0xDEADBEEF, ArticleCount: 7},
		{FileIndex: 1, Complete: false, FetchPolicy: 1, Filename: "b.bin", AssembledCRC32: 0, ArticleCount: 3},
	}
	if err := repo.Add(t.Context(), retainedEntry("job-round-trip"), want); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := repo.RetainedFiles(t.Context(), "job-round-trip")
	if err != nil {
		t.Fatalf("RetainedFiles: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read back %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestAdd_RollsBackTheEntryWhenItsProgressCannotBeStored pins the guarantee
// Add's doc states: an entry must not land without the progress it implies,
// because a retry reads a bare entry as "nothing was ever downloaded" and
// re-fetches the whole job.
//
// The forced failure is two rows claiming one file index, which
// PRIMARY KEY (job_id, file_index) refuses. The entry must not survive it.
func TestAdd_RollsBackTheEntryWhenItsProgressCannotBeStored(t *testing.T) {
	_, repo := openTestDB(t)

	duplicate := []FileProgress{
		{FileIndex: 0, ArticleCount: 1},
		{FileIndex: 0, ArticleCount: 1},
	}
	if err := repo.Add(t.Context(), retainedEntry("job-rollback"), duplicate); err == nil {
		t.Fatal("Add reported success for progress the schema refuses")
	}

	if _, err := repo.Get(t.Context(), "job-rollback"); err == nil {
		t.Error("the history entry survived a failed progress write; a retry would read no " +
			"retained progress and re-fetch every article the attempt had already written")
	}
	rows, err := repo.RetainedFiles(t.Context(), "job-rollback")
	if err != nil {
		t.Fatalf("RetainedFiles: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d retained rows survived the rollback, want 0", len(rows))
	}
}

// TestAdd_WithNoProgressStoresJustTheEntry keeps the common case honest: a
// completed job carries no retained progress, and that is not an error.
func TestAdd_WithNoProgressStoresJustTheEntry(t *testing.T) {
	_, repo := openTestDB(t)

	if err := repo.Add(t.Context(), retainedEntry("job-bare"), nil); err != nil {
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
		t.Errorf("got %d retained rows for an entry added without any, want 0", len(rows))
	}
}

// TestDelete_TakesRetainedProgressWithTheEntry pins the other half of the
// ownership: this package removes an entry and its retained progress in one
// transaction. That is what stands in for a foreign key on history(nzo_id) —
// the lifecycle is stated once, here in Go, rather than split between the code
// and the schema.
func TestDelete_TakesRetainedProgressWithTheEntry(t *testing.T) {
	_, repo := openTestDB(t)

	files := []FileProgress{{FileIndex: 0, Complete: true, ArticleCount: 2}}
	if err := repo.Add(t.Context(), retainedEntry("job-delete"), files); err != nil {
		t.Fatalf("Add: %v", err)
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
