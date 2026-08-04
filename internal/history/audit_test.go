package history

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Prune – mixed ages and statuses
// ---------------------------------------------------------------------------

// TestPrune_MixedAgesAndStatuses inserts records spanning a range of ages and
// statuses, prunes with both retainDays and retainFailedDays, and verifies
// that only the expected records survive.
func TestPrune_MixedAgesAndStatuses(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	now := time.Now().Truncate(time.Second).UTC()

	entries := []struct {
		id     string
		status string
		age    int // days ago
		keep   bool
	}{
		// Non-failed: retainDays = 7 → keep if ≤7 days old.
		{"nf_old", "Completed", 30, false},
		{"nf_boundary", "Completed", 8, false},
		{"nf_recent", "Completed", 3, true},
		{"nf_today", "Completed", 0, true},
		// Failed: retainFailedDays = 14 → keep if ≤14 days old.
		{"f_old", "Failed", 60, false},
		{"f_boundary", "Failed", 15, false},
		{"f_recent", "Failed", 5, true},
		{"f_today", "Failed", 0, true},
	}

	for _, tc := range entries {
		e := sampleEntry(tc.id, "show-"+tc.id, tc.status, "TV")
		e.Completed = now.AddDate(0, 0, -tc.age)
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add %s: %v", tc.id, err)
		}
	}

	pruned := pruneVia(t, repo, 7, 14)

	wantPruned := 0
	for _, tc := range entries {
		if !tc.keep {
			wantPruned++
		}
	}
	if pruned != wantPruned {
		t.Errorf("pruned = %d, want %d", pruned, wantPruned)
	}

	for _, tc := range entries {
		_, err := repo.Get(ctx, tc.id)
		if tc.keep {
			if err != nil {
				t.Errorf("%s (age=%dd, status=%s): should survive prune, got error: %v",
					tc.id, tc.age, tc.status, err)
			}
		} else {
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s (age=%dd, status=%s): should have been pruned, got: %v",
					tc.id, tc.age, tc.status, err)
			}
		}
	}
}

// TestPrune_OnlyNonFailedRetention verifies that when only retainDays is set
// (retainFailedDays = 0), failed entries are kept forever regardless of age.
func TestPrune_OnlyNonFailedRetention(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	ancient := time.Now().AddDate(-1, 0, 0).Truncate(time.Second).UTC()

	// Old completed entry — should be pruned.
	e1 := sampleEntry("comp_old", "Old Completed", "Completed", "TV")
	e1.Completed = ancient

	// Old failed entry — should survive (retainFailedDays=0 means keep forever).
	e2 := sampleEntry("fail_old", "Old Failed", "Failed", "TV")
	e2.Completed = ancient

	for _, e := range []Entry{e1, e2} {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	pruned := pruneVia(t, repo, 30, 0)
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1", pruned)
	}

	if _, err := repo.Get(ctx, "comp_old"); !errors.Is(err, ErrNotFound) {
		t.Error("old completed should have been pruned")
	}
	if _, err := repo.Get(ctx, "fail_old"); err != nil {
		t.Errorf("old failed should survive (retainFailedDays=0): %v", err)
	}
}

// TestPrune_OnlyFailedRetention verifies that when only retainFailedDays is
// set (retainDays = 0), non-failed entries are kept forever.
func TestPrune_OnlyFailedRetention(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	ancient := time.Now().AddDate(-1, 0, 0).Truncate(time.Second).UTC()

	e1 := sampleEntry("comp_ancient", "Ancient Completed", "Completed", "TV")
	e1.Completed = ancient

	e2 := sampleEntry("fail_ancient", "Ancient Failed", "Failed", "TV")
	e2.Completed = ancient

	for _, e := range []Entry{e1, e2} {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	pruned := pruneVia(t, repo, 0, 30)
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1", pruned)
	}

	if _, err := repo.Get(ctx, "comp_ancient"); err != nil {
		t.Errorf("ancient completed should survive (retainDays=0): %v", err)
	}
	if _, err := repo.Get(ctx, "fail_ancient"); !errors.Is(err, ErrNotFound) {
		t.Error("ancient failed should have been pruned")
	}
}

// ---------------------------------------------------------------------------
// Delete – chunked deletion (>999 IDs)
// ---------------------------------------------------------------------------

// TestDelete_ChunkedDeletion creates 1005 records and deletes them all in a
// single call to verify that the chunked deletion logic (chunkSize = 999)
// correctly processes multiple batches within one transaction.
func TestDelete_ChunkedDeletion(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	const total = 1005
	ids := make([]string, total)
	for i := range total {
		id := fmt.Sprintf("chunk_%04d", i)
		ids[i] = id
		e := sampleEntry(id, "show", "Completed", "TV")
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}

	// Verify all records were inserted.
	count, err := repo.Count(ctx, SearchOptions{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != total {
		t.Fatalf("inserted count = %d, want %d", count, total)
	}

	// Delete all 1005 at once — this must chunk internally.
	deleted, err := repo.Delete(ctx, ids...)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted != total {
		t.Errorf("deleted = %d, want %d", deleted, total)
	}

	// Verify the table is empty.
	remaining, err := repo.Count(ctx, SearchOptions{})
	if err != nil {
		t.Fatalf("Count after delete: %v", err)
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0", remaining)
	}
}

// TestDelete_ChunkedPartialMatch verifies chunked deletion when some IDs in
// the batch don't exist. The returned count should reflect only rows actually
// deleted.
func TestDelete_ChunkedPartialMatch(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	const realCount = 500
	ids := make([]string, 0, 1200)

	// Insert 500 real records.
	for i := range realCount {
		id := fmt.Sprintf("real_%04d", i)
		ids = append(ids, id)
		e := sampleEntry(id, "show", "Completed", "TV")
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}

	// Add 700 ghost IDs that don't exist in the DB.
	for i := range 700 {
		ids = append(ids, fmt.Sprintf("ghost_%04d", i))
	}

	deleted, err := repo.Delete(ctx, ids...)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted != realCount {
		t.Errorf("deleted = %d, want %d", deleted, realCount)
	}
}

// TestDelete_ZeroIDs verifies that Delete with an empty slice is a no-op
// that returns (0, nil) without touching the database.
func TestDelete_ZeroIDs(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	// Insert one record to prove the no-op doesn't delete anything.
	e := sampleEntry("survivor", "show", "Completed", "TV")
	if err := repo.Add(ctx, e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	n, err := repo.Delete(ctx)
	if err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d, want 0", n)
	}

	// The record should still exist.
	if _, err := repo.Get(ctx, "survivor"); err != nil {
		t.Errorf("record should survive zero-ID delete: %v", err)
	}
}

// ---------------------------------------------------------------------------
// MarkCompleted – state change verification
// ---------------------------------------------------------------------------

// TestMarkCompleted_StateChange inserts a Failed record, marks it completed,
// and verifies both the status change and the completed timestamp update.
func TestMarkCompleted_StateChange(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	before := time.Now().Truncate(time.Second).UTC()

	e := sampleEntry("mc_audit", "My Failed Show", "Failed", "TV")
	e.Completed = time.Time{} // zero — not yet completed
	e.FailMessage = "connection timed out"
	if err := repo.Add(ctx, e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Verify initial state.
	got, err := repo.Get(ctx, "mc_audit")
	if err != nil {
		t.Fatalf("Get (before): %v", err)
	}
	if got.Status != "Failed" {
		t.Fatalf("initial Status = %q, want Failed", got.Status)
	}
	if !got.Completed.IsZero() {
		t.Fatalf("initial Completed should be zero, got %v", got.Completed)
	}

	// Mark completed.
	if err := repo.MarkCompleted(ctx, "mc_audit"); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}

	after := time.Now().Add(time.Second).Truncate(time.Second).UTC()

	// Verify updated state.
	got, err = repo.Get(ctx, "mc_audit")
	if err != nil {
		t.Fatalf("Get (after): %v", err)
	}
	if got.Status != "Completed" {
		t.Errorf("Status = %q, want Completed", got.Status)
	}
	if got.Completed.Before(before) || got.Completed.After(after) {
		t.Errorf("Completed = %v, want between %v and %v", got.Completed, before, after)
	}
	// FailMessage should be preserved — MarkCompleted only touches status and completed.
	if got.FailMessage != "connection timed out" {
		t.Errorf("FailMessage = %q, want %q", got.FailMessage, "connection timed out")
	}
}

// TestMarkCompleted_AlreadyCompleted verifies that marking an already-completed
// entry updates the completed timestamp without error.
func TestMarkCompleted_AlreadyCompleted(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	e := sampleEntry("mc_idem", "Already Done", "Completed", "TV")
	e.Completed = time.Now().Add(-24 * time.Hour).Truncate(time.Second).UTC()
	if err := repo.Add(ctx, e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	before := time.Now().Truncate(time.Second).UTC()

	if err := repo.MarkCompleted(ctx, "mc_idem"); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}

	got, err := repo.Get(ctx, "mc_idem")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "Completed" {
		t.Errorf("Status = %q, want Completed", got.Status)
	}
	// Completed timestamp should have been updated to ~now.
	if got.Completed.Before(before) {
		t.Errorf("Completed = %v, should be ≥ %v", got.Completed, before)
	}
}

// TestMarkCompleted_MissingID verifies that marking a nonexistent nzo_id
// returns ErrNotFound.
func TestMarkCompleted_MissingID(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	err := repo.MarkCompleted(ctx, "does_not_exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkCompleted missing: got %v, want ErrNotFound", err)
	}
}
