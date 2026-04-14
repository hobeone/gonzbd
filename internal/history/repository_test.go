package history

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// openTestDB creates a fresh database in t's temp directory and returns an
// open *DB and matching *Repository. The DB is closed automatically when the
// test ends.
func openTestDB(t *testing.T) (*DB, *Repository) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history1.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db, NewRepository(db)
}

// sampleEntry builds a fully-populated Entry for round-trip tests. Fields that
// would normally be empty in production can remain zero-valued.
func sampleEntry(nzoID, name, status, category string) Entry {
	return Entry{
		NzoID:        nzoID,
		Name:         name,
		NzbName:      name + ".nzb",
		Category:     category,
		Status:       status,
		Completed:    time.Now().Truncate(time.Second).UTC(),
		TimeAdded:    time.Now().Add(-time.Hour).Truncate(time.Second).UTC(),
		Bytes:        1_234_567,
		Downloaded:   1_234_567,
		DownloadTime: 42,
		PostprocTime: 10,
		StageLog:     `[{"name":"Download","result":"OK"}]`,
		ScriptLog:    []byte("script output"),
		ScriptLine:   "last line",
		FailMessage:  "",
	}
}

func TestAddGetRoundTrip(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	want := sampleEntry("SABnzbd_nzo_abc123", "My Show S01E01", "Completed", "TV")
	if err := repo.Add(ctx, want); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := repo.Get(ctx, want.NzoID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"NzoID", got.NzoID, want.NzoID},
		{"Name", got.Name, want.Name},
		{"NzbName", got.NzbName, want.NzbName},
		{"Category", got.Category, want.Category},
		{"Status", got.Status, want.Status},
		{"Bytes", got.Bytes, want.Bytes},
		{"Downloaded", got.Downloaded, want.Downloaded},
		{"DownloadTime", got.DownloadTime, want.DownloadTime},
		{"StageLog", got.StageLog, want.StageLog},
		{"ScriptLine", got.ScriptLine, want.ScriptLine},
		{"Completed", got.Completed.Unix(), want.Completed.Unix()},
		{"TimeAdded", got.TimeAdded.Unix(), want.TimeAdded.Unix()},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.field, c.got, c.want)
		}
	}
	// ScriptLog is a []byte field.
	if string(got.ScriptLog) != string(want.ScriptLog) {
		t.Errorf("ScriptLog: got %q, want %q", got.ScriptLog, want.ScriptLog)
	}
}

func TestAddDuplicateNzoIDErrors(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	e := sampleEntry("dup_id", "show", "Completed", "TV")
	if err := repo.Add(ctx, e); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	err := repo.Add(ctx, e)
	if err == nil {
		t.Fatal("second Add with duplicate nzo_id should error")
	}
}

func TestGetMissingReturnsErrNotFound(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	_, err := repo.Get(ctx, "does_not_exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: got %v, want ErrNotFound", err)
	}
}

func TestSearchByStatus(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	entries := []Entry{
		sampleEntry("id1", "show1", "Completed", "TV"),
		sampleEntry("id2", "show2", "Failed", "TV"),
		sampleEntry("id3", "show3", "Completed", "Movies"),
	}
	for _, e := range entries {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add %s: %v", e.NzoID, err)
		}
	}

	tests := []struct {
		name   string
		status string
		want   int
	}{
		{"completed only", "Completed", 2},
		{"failed only", "Failed", 1},
		{"no match", "Downloading", 0},
		{"no filter", "", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.Search(ctx, SearchOptions{Status: tc.status})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("len = %d, want %d", len(got), tc.want)
			}
		})
	}
}

func TestSearchByCategory(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	for _, e := range []Entry{
		sampleEntry("c1", "show1", "Completed", "TV"),
		sampleEntry("c2", "movie1", "Completed", "Movies"),
		sampleEntry("c3", "show2", "Completed", "TV"),
	} {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	got, err := repo.Search(ctx, SearchOptions{Category: "TV"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

func TestSearchBySubstring(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	for _, e := range []Entry{
		sampleEntry("s1", "Breaking Bad S01E01", "Completed", "TV"),
		sampleEntry("s2", "Better Call Saul S01E01", "Completed", "TV"),
		sampleEntry("s3", "Sopranos S01E01", "Completed", "TV"),
	} {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	tests := []struct {
		search string
		want   int
	}{
		{"Breaking", 1},
		{"S01E01", 3},
		{"Saul", 1},
		{"Dexter", 0},
	}
	for _, tc := range tests {
		t.Run(tc.search, func(t *testing.T) {
			got, err := repo.Search(ctx, SearchOptions{Search: tc.search})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("len = %d, want %d", len(got), tc.want)
			}
		})
	}
}

func TestSearchPagination(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	for i := range 10 {
		e := sampleEntry(
			"page_id_"+string(rune('a'+i)),
			"Item",
			"Completed", "TV",
		)
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	tests := []struct {
		name  string
		start int
		limit int
		want  int
	}{
		{"first page of 3", 0, 3, 3},
		{"second page of 3", 3, 3, 3},
		{"last partial page", 9, 3, 1},
		{"no limit", 0, 0, 10},
		{"start past end", 20, 5, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.Search(ctx, SearchOptions{Start: tc.start, Limit: tc.limit})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("len = %d, want %d", len(got), tc.want)
			}
		})
	}
}

func TestDeleteSingle(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	e := sampleEntry("del1", "show", "Completed", "TV")
	if err := repo.Add(ctx, e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	n, err := repo.Delete(ctx, "del1")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}

	_, err = repo.Get(ctx, "del1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete: %v, want ErrNotFound", err)
	}
}

func TestDeleteMultiple(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	ids := []string{"dm1", "dm2", "dm3"}
	for _, id := range ids {
		e := sampleEntry(id, "show", "Completed", "TV")
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}

	n, err := repo.Delete(ctx, "dm1", "dm3")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2", n)
	}

	// dm2 must survive.
	if _, err := repo.Get(ctx, "dm2"); err != nil {
		t.Errorf("Get surviving entry: %v", err)
	}
}

func TestDeleteUnknownIDsReturnsZero(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	n, err := repo.Delete(ctx, "ghost1", "ghost2")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d, want 0", n)
	}
}

func TestDeleteNoIDsIsNoop(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	n, err := repo.Delete(ctx)
	if err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d, want 0", n)
	}
}

func TestMarkCompleted(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	before := time.Now().Truncate(time.Second)
	e := sampleEntry("mc1", "show", "Failed", "TV")
	e.Completed = time.Time{} // start with zero
	if err := repo.Add(ctx, e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := repo.MarkCompleted(ctx, "mc1"); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}

	got, err := repo.Get(ctx, "mc1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "Completed" {
		t.Errorf("Status = %q, want Completed", got.Status)
	}
	if got.Completed.Before(before) {
		t.Errorf("Completed %v is before test start %v", got.Completed, before)
	}
}

func TestMarkCompletedMissingReturnsErrNotFound(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	err := repo.MarkCompleted(ctx, "no_such_id")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkCompleted missing: got %v, want ErrNotFound", err)
	}
}

func TestPruneRespectsRetainDays(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	old := sampleEntry("old1", "old show", "Completed", "TV")
	old.Completed = time.Now().AddDate(0, 0, -10) // 10 days ago
	recent := sampleEntry("recent1", "recent show", "Completed", "TV")
	recent.Completed = time.Now().AddDate(0, 0, -1) // 1 day ago

	for _, e := range []Entry{old, recent} {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	n, err := repo.Prune(ctx, 5, 0) // retain 5 days of non-failed
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned = %d, want 1", n)
	}

	if _, err := repo.Get(ctx, "old1"); !errors.Is(err, ErrNotFound) {
		t.Error("old entry should have been pruned")
	}
	if _, err := repo.Get(ctx, "recent1"); err != nil {
		t.Errorf("recent entry should survive: %v", err)
	}
}

func TestPruneRespectsRetainFailedDays(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	oldFailed := sampleEntry("of1", "old failed", "Failed", "TV")
	oldFailed.Completed = time.Now().AddDate(0, 0, -20)
	recentFailed := sampleEntry("rf1", "recent failed", "Failed", "TV")
	recentFailed.Completed = time.Now().AddDate(0, 0, -2)
	// A non-failed old entry that should survive because retainDays=0.
	oldCompleted := sampleEntry("oc1", "old completed", "Completed", "TV")
	oldCompleted.Completed = time.Now().AddDate(0, 0, -20)

	for _, e := range []Entry{oldFailed, recentFailed, oldCompleted} {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	// 0 retainDays = keep non-failed forever; 7 retainFailedDays = purge old failed.
	n, err := repo.Prune(ctx, 0, 7)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned = %d, want 1", n)
	}

	if _, err := repo.Get(ctx, "of1"); !errors.Is(err, ErrNotFound) {
		t.Error("old failed entry should have been pruned")
	}
	if _, err := repo.Get(ctx, "rf1"); err != nil {
		t.Errorf("recent failed should survive: %v", err)
	}
	if _, err := repo.Get(ctx, "oc1"); err != nil {
		t.Errorf("old completed should survive (retainDays=0): %v", err)
	}
}

func TestPruneZeroZeroIsNoop(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := context.Background()

	e := sampleEntry("noop1", "show", "Completed", "TV")
	e.Completed = time.Now().AddDate(0, 0, -100)
	if err := repo.Add(ctx, e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	n, err := repo.Prune(ctx, 0, 0)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned = %d, want 0 (both zero = keep forever)", n)
	}

	if _, err := repo.Get(ctx, "noop1"); err != nil {
		t.Errorf("entry should survive Prune(0,0): %v", err)
	}
}
