package history

import (
	"context"
	"testing"
	"time"
)

// ---------- ListIDs ----------

func TestListIDs_ReturnsAll(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	repo.Add(ctx, sampleEntry("nzo_1", "Job1", "Completed", "TV"))
	repo.Add(ctx, sampleEntry("nzo_2", "Job2", "Failed", "Movies"))
	repo.Add(ctx, sampleEntry("nzo_3", "Job3", "Completed", "TV"))

	ids, err := repo.ListIDs(ctx, SearchOptions{})
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("got %d IDs, want 3", len(ids))
	}
}

func TestListIDs_Filtered(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	repo.Add(ctx, sampleEntry("nzo_a", "A", "Completed", "TV"))
	repo.Add(ctx, sampleEntry("nzo_b", "B", "Failed", "Movies"))

	ids, err := repo.ListIDs(ctx, SearchOptions{Status: "Failed"})
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("got %d IDs, want 1", len(ids))
	}
	if ids[0] != "nzo_b" {
		t.Errorf("got %q, want nzo_b", ids[0])
	}
}

func TestListIDs_Empty(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	ids, err := repo.ListIDs(ctx, SearchOptions{})
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("got %d IDs, want 0", len(ids))
	}
}

// ---------- Count ----------

func TestCount_All(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	repo.Add(ctx, sampleEntry("nzo_1", "J1", "Completed", "TV"))
	repo.Add(ctx, sampleEntry("nzo_2", "J2", "Failed", "Movies"))
	repo.Add(ctx, sampleEntry("nzo_3", "J3", "Completed", "TV"))

	count, err := repo.Count(ctx, SearchOptions{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestCount_Filtered(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	repo.Add(ctx, sampleEntry("nzo_1", "J1", "Completed", "TV"))
	repo.Add(ctx, sampleEntry("nzo_2", "J2", "Failed", "Movies"))

	count, err := repo.Count(ctx, SearchOptions{Status: "Completed"})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestCount_Category(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	repo.Add(ctx, sampleEntry("nzo_1", "J1", "Completed", "TV"))
	repo.Add(ctx, sampleEntry("nzo_2", "J2", "Completed", "Movies"))
	repo.Add(ctx, sampleEntry("nzo_3", "J3", "Completed", "TV"))

	count, err := repo.Count(ctx, SearchOptions{Category: "TV"})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (TV only)", count)
	}
}

func TestCount_Empty(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	count, err := repo.Count(ctx, SearchOptions{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

// ---------- Search edge cases ----------

func TestSearch_ArchiveOnly(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	e1 := sampleEntry("nzo_1", "Normal", "Completed", "TV")
	e1.Archive = 0
	repo.Add(ctx, e1)

	e2 := sampleEntry("nzo_2", "Archived", "Completed", "TV")
	e2.Archive = 1
	repo.Add(ctx, e2)

	results, err := repo.Search(ctx, SearchOptions{ArchiveOnly: true})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].NzoID != "nzo_2" {
		t.Errorf("got %q, want nzo_2", results[0].NzoID)
	}
}

func TestSearch_MD5Sum(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	e1 := sampleEntry("nzo_1", "J1", "Completed", "TV")
	e1.MD5Sum = "abc123"
	repo.Add(ctx, e1)

	e2 := sampleEntry("nzo_2", "J2", "Completed", "TV")
	e2.MD5Sum = "def456"
	repo.Add(ctx, e2)

	results, err := repo.Search(ctx, SearchOptions{MD5Sum: "abc123"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].NzoID != "nzo_1" {
		t.Errorf("got %q, want nzo_1", results[0].NzoID)
	}
}

func TestSearch_Pagination(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		e := sampleEntry("nzo_"+string(rune('a'+i)), "Job", "Completed", "TV")
		e.Completed = time.Now().Add(-time.Duration(i) * time.Hour).Truncate(time.Second).UTC()
		repo.Add(ctx, e)
	}

	// Get page 2 (start=2, limit=2).
	results, err := repo.Search(ctx, SearchOptions{Start: 2, Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("got %d results, want 2", len(results))
	}
}

func TestSearch_OffsetWithoutLimit(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		e := sampleEntry("nzo_"+string(rune('a'+i)), "Job", "Completed", "TV")
		e.Completed = time.Now().Add(-time.Duration(i) * time.Hour).Truncate(time.Second).UTC()
		repo.Add(ctx, e)
	}

	// Offset 1, no limit (should return 2 results).
	results, err := repo.Search(ctx, SearchOptions{Start: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("got %d results, want 2", len(results))
	}
}

// ---------- fromUnix / toUnix ----------

func TestFromUnix_Zero(t *testing.T) {
	t.Parallel()
	got := fromUnix(0)
	if !got.IsZero() {
		t.Errorf("fromUnix(0) = %v, want zero time", got)
	}
}

func TestFromUnix_NonZero(t *testing.T) {
	t.Parallel()
	got := fromUnix(1714272000) // 2024-04-28 00:00:00 UTC
	if got.IsZero() {
		t.Error("fromUnix(1714272000) should not be zero")
	}
	if got.UTC().Year() != 2024 {
		t.Errorf("year = %d, want 2024", got.UTC().Year())
	}
}

func TestToUnix_Zero(t *testing.T) {
	t.Parallel()
	got := toUnix(time.Time{})
	if got != 0 {
		t.Errorf("toUnix(zero) = %d, want 0", got)
	}
}

func TestToUnix_RoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second).UTC()
	ts := toUnix(now)
	back := fromUnix(ts)
	if !back.Equal(now) {
		t.Errorf("round-trip: %v != %v", back, now)
	}
}

// ---------- MarkCompleted for missing entry ----------

func TestMarkCompleted_NotFound(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	err := repo.MarkCompleted(ctx, "nonexistent_nzo")
	if err == nil {
		t.Error("expected error for nonexistent nzoID")
	}
}

// ---------- buildWhereClause ----------

func TestBuildWhereClause_AllFilters(t *testing.T) {
	t.Parallel()
	opts := SearchOptions{
		Status:      "Completed",
		Category:    "TV",
		Search:      "show",
		ArchiveOnly: true,
		MD5Sum:      "abc",
	}
	where, args := buildWhereClause(opts)
	if len(where) != 5 {
		t.Errorf("got %d WHERE clauses, want 5", len(where))
	}
	// args: status, category, name LIKE, nzb_name LIKE, md5sum = 5 but search uses 2 args
	if len(args) != 5 {
		t.Errorf("got %d args, want 5", len(args))
	}
}

func TestBuildWhereClause_Empty(t *testing.T) {
	t.Parallel()
	where, args := buildWhereClause(SearchOptions{})
	if len(where) != 0 {
		t.Errorf("got %d WHERE clauses, want 0", len(where))
	}
	if len(args) != 0 {
		t.Errorf("got %d args, want 0", len(args))
	}
}
