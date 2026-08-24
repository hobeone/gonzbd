package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
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
	db, err := Open(t.Context(), path)
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
	ctx := t.Context()

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
	ctx := t.Context()

	e := sampleEntry("dup_id", "show", "Completed", "TV")
	if err := repo.Add(ctx, e); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	err := repo.Add(ctx, e)
	if err == nil {
		t.Fatal("second Add with duplicate nzo_id should error")
	}
}

// TestScanEntry_NullColumnsRoundTrip proves TRACE-4: a row with NULL
// TEXT/INTEGER columns (as the nullable schema in
// internal/history/migrations/001_initial.sql permits, even though Add
// always writes concrete zero values) must round-trip through Get/Search
// without erroring, with NULL coalescing to the Go zero value.
func TestScanEntry_NullColumnsRoundTrip(t *testing.T) {
	db, repo := openTestDB(t)
	ctx := t.Context()

	// Insert a row with every nullable column left NULL, bypassing Add
	// (which always binds concrete values) to simulate a row written by
	// another means (manual sqlite3, a future migration, an external tool).
	_, err := db.db.ExecContext(ctx,
		`INSERT INTO history (id, nzo_id, script_log) VALUES (?, ?, ?)`,
		1, "null_row", []byte(nil))
	if err != nil {
		t.Fatalf("insert NULL row: %v", err)
	}

	got, err := repo.Get(ctx, "null_row")
	if err != nil {
		t.Fatalf("Get on row with NULL columns: %v", err)
	}
	if got.NzoID != "null_row" {
		t.Errorf("NzoID = %q, want %q", got.NzoID, "null_row")
	}
	// Every other nullable TEXT/INTEGER column should coalesce to its Go
	// zero value rather than erroring the scan.
	if got.Name != "" || got.Category != "" || got.Status != "" || got.StageLog != "" {
		t.Errorf("expected NULL text columns to coalesce to \"\", got Name=%q Category=%q Status=%q StageLog=%q",
			got.Name, got.Category, got.Status, got.StageLog)
	}
	if got.Bytes != 0 || got.DownloadTime != 0 || got.Archive != 0 {
		t.Errorf("expected NULL integer columns to coalesce to 0, got Bytes=%d DownloadTime=%d Archive=%d",
			got.Bytes, got.DownloadTime, got.Archive)
	}
	if !got.Completed.IsZero() || !got.TimeAdded.IsZero() {
		t.Errorf("expected NULL timestamp columns to coalesce to zero time, got Completed=%v TimeAdded=%v",
			got.Completed, got.TimeAdded)
	}

	// Also exercise Search, which shares scanEntry with Get.
	results, err := repo.Search(ctx, SearchOptions{})
	if err != nil {
		t.Fatalf("Search over row with NULL columns: %v", err)
	}
	if len(results) != 1 || results[0].NzoID != "null_row" {
		t.Errorf("Search results = %+v, want single null_row entry", results)
	}
}

func TestGetMissingReturnsErrNotFound(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	_, err := repo.Get(ctx, "does_not_exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: got %v, want ErrNotFound", err)
	}
}

func TestSearchByStatus(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

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
	ctx := t.Context()

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
	ctx := t.Context()

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

// TestSearchBySubstring_LIKEWildcardEscaping verifies that SQL LIKE
// wildcards (%, _) in user search input are treated as literal characters
// rather than pattern metacharacters.
func TestSearchBySubstring_LIKEWildcardEscaping(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	for _, e := range []Entry{
		sampleEntry("esc1", "100% Complete", "Completed", "TV"),
		sampleEntry("esc2", "file_name_test", "Completed", "TV"),
		sampleEntry("esc3", "normal show", "Completed", "TV"),
	} {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	tests := []struct {
		name   string
		search string
		want   int
	}{
		{"literal percent", "100%", 1},
		{"literal underscore", "file_name", 1},
		{"percent alone matches entry with percent", "%", 1},           // Matches "100% Complete" which contains a literal "%"
		{"bare underscore only matches literal underscores", "e_n", 1}, // Without escaping, "e_n" would match "e" + any char + "n" in all entries
		{"no match for non-existent literal wildcard", "100%xxx", 0},   // No entry matches this
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
	ctx := t.Context()

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

// TestSearch_PreallocationCapBounded proves the result-slice preallocation
// hint is capped independently of opts.Limit. Search still returns the
// correct rows either way; this test is only about the allocation hint,
// which is not directly observable except via cap() on an empty result
// (nothing appended beyond the initial make), so it uses an empty DB and
// checks cap() of the returned (empty) slice.
func TestSearch_PreallocationCapBounded(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	tests := []struct {
		name    string
		limit   int
		wantCap int
	}{
		{"unbounded search falls back to the small default", 0, 16},
		{"bounded, reasonable limit preallocates to it", 5, 5},
		{"huge limit is clamped, not preallocated in full", 50_000_000, 10_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.Search(ctx, SearchOptions{Limit: tc.limit})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if cap(got) != tc.wantCap {
				t.Errorf("cap = %d, want %d", cap(got), tc.wantCap)
			}
		})
	}
}

func TestDeleteSingle(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

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
	ctx := t.Context()

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
	ctx := t.Context()

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
	ctx := t.Context()

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
	ctx := t.Context()

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
	ctx := t.Context()

	err := repo.MarkCompleted(ctx, "no_such_id")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkCompleted missing: got %v, want ErrNotFound", err)
	}
}

func TestPruneRespectsRetainDays(t *testing.T) {
	_, repo := openTestDB(t)
	ctx := t.Context()

	old := sampleEntry("old1", "old show", "Completed", "TV")
	old.Completed = time.Now().AddDate(0, 0, -10) // 10 days ago
	recent := sampleEntry("recent1", "recent show", "Completed", "TV")
	recent.Completed = time.Now().AddDate(0, 0, -1) // 1 day ago

	for _, e := range []Entry{old, recent} {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	n := pruneVia(t, repo, 5, 0) // retain 5 days of non-failed
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
	ctx := t.Context()

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
	n := pruneVia(t, repo, 0, 7)
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
	ctx := t.Context()

	e := sampleEntry("noop1", "show", "Completed", "TV")
	e.Completed = time.Now().AddDate(0, 0, -100)
	if err := repo.Add(ctx, e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	n := pruneVia(t, repo, 0, 0)
	if n != 0 {
		t.Errorf("pruned = %d, want 0 (both zero = keep forever)", n)
	}

	if _, err := repo.Get(ctx, "noop1"); err != nil {
		t.Errorf("entry should survive Prune(0,0): %v", err)
	}
}

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"normal", "normal"},
		{"percent%", `percent\%`},
		{"underscore_", `underscore\_`},
		{"backslash\\", `backslash\\`},
		{"all%_\\chars", `all\%\_\\chars`},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			if got := escapeLike(tc.input); got != tc.want {
				t.Errorf("escapeLike(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestDBConnectionSettings(t *testing.T) {
	db, _ := openTestDB(t)
	if got := db.db.Stats().MaxOpenConnections; got != 25 {
		t.Errorf("MaxOpenConnections = %d, want 25", got)
	}
}

func TestDeleteChunked(t *testing.T) {
	db, repo := openTestDB(t)
	ctx := t.Context()

	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	const totalEntries = 999
	ids := make([]string, totalEntries)
	for i := range totalEntries {
		ids[i] = fmt.Sprintf("chunk-%d", i)
		_, err := tx.ExecContext(ctx, "INSERT INTO history (nzo_id) VALUES (?)", ids[i])
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	n, err := repo.Delete(ctx, ids...)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n != totalEntries {
		t.Errorf("deleted = %d, want %d", n, totalEntries)
	}
}

func TestOpen_Error(t *testing.T) {
	// Attempting to open a directory as a SQLite file should fail
	_, err := Open(t.Context(), t.TempDir())
	if err == nil {
		t.Error("expected error opening directory as database, got nil")
	}
}

func TestOpen_ReadOnlyError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "readonly.db")

	// Create and initialize a valid SQLite file
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open setup: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close setup: %v", err)
	}

	// Make the file read-only
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	// Open again. Ping should succeed, but Exec(WAL) or Exec(VACUUM) should fail.
	_, err = Open(t.Context(), path)
	if err == nil {
		t.Error("expected error opening read-only database, got nil")
	}
}

func TestOpen_GooseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "goose_error.db")

	// Create a database and pre-create the 'history' table manually
	// so that the goose migration trying to create the same table fails.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec("CREATE TABLE history (id INTEGER PRIMARY KEY)")
	if err != nil {
		db.Close()
		t.Fatalf("CREATE TABLE: %v", err)
	}
	db.Close()

	_, err = Open(t.Context(), path)
	if err == nil {
		t.Error("expected error from goose.Up when table already exists, got nil")
	}
}

func TestRepository_Ping(t *testing.T) {
	var nilRepo *Repository
	if err := nilRepo.Ping(t.Context()); err == nil {
		t.Error("expected error from nil repository Ping, got nil")
	}

	_, repo := openTestDB(t)
	if err := repo.Ping(t.Context()); err != nil {
		t.Errorf("repo.Ping: %v", err)
	}
}

func TestRepository_DB(t *testing.T) {
	_, repo := openTestDB(t)
	if repo.DB() == nil {
		t.Error("expected non-nil sql.DB from repo.DB()")
	}
}

// TestDelete_SweepsRetainedDurabilityRows pins the bound on the retention that
// keeps a retry from truncating away its own partial file.
//
// A failed job's durable_runs and failed_articles survive MoveToHistory on
// purpose: a retry reuses the job ID and needs the runs to bound its truncate
// to the whole partial rather than to the articles it re-fetched, and the
// failed rows to avoid re-attempting what already failed permanently. From
// that point they are owned by the history entry, and like history_job_files
// they have no foreign key to cascade from, so nothing else would ever remove
// them. Without this they accumulate one set per failed job for the life of
// the database.
func TestDelete_SweepsRetainedDurabilityRows(t *testing.T) {
	ctx := context.Background()
	db, repo := openTestDB(t)

	if err := repo.Add(ctx, Entry{NzoID: "job-a", Name: "a", Status: "Failed"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO durable_runs (job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32)
		   VALUES ('job-a',0,0,0,0,100,7)`,
		`INSERT INTO failed_articles (job_id, art_idx) VALUES ('job-a',1)`,
	} {
		if _, err := db.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	count := func(table string) int {
		t.Helper()
		var n int
		if err := db.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE job_id = 'job-a'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Grounding: the seed must have landed, or the assertions below hold
	// against an empty table for a reason unrelated to the sweep.
	if count("durable_runs") != 1 || count("failed_articles") != 1 {
		t.Fatalf("fixture seeded %d runs and %d failed rows, want 1 and 1",
			count("durable_runs"), count("failed_articles"))
	}

	if _, err := repo.Delete(ctx, "job-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if n := count("durable_runs"); n != 0 {
		t.Errorf("%d durable_runs rows survive their history entry; nothing else "+
			"removes them, so they accumulate one set per failed job forever", n)
	}
	if n := count("failed_articles"); n != 0 {
		t.Errorf("%d failed_articles rows survive their history entry", n)
	}
}

// fixedScanner assigns values into scanEntry's dest list by POSITION, which is
// the only way to exercise what that function actually risks getting wrong.
type fixedScanner struct {
	at  map[int]any
	err error
}

func (f fixedScanner) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	for i, d := range dest {
		v, ok := f.at[i]
		if !ok {
			continue // left as the zero value, i.e. a NULL column
		}
		switch p := d.(type) {
		case *sql.NullString:
			*p = sql.NullString{String: v.(string), Valid: true}
		case *sql.NullInt64:
			*p = sql.NullInt64{Int64: v.(int64), Valid: true}
		case *string:
			*p = v.(string)
		case *int64:
			*p = v.(int64)
		}
	}
	return nil
}

// TestScanEntry_MapsColumnsByPosition pins scanEntry against the failure it is
// actually exposed to. TestScanEntry_NullColumnsRoundTrip above already covers
// NULL collapsing, and does it better — through a real row and the real driver
// — so this deliberately does not restate it.
//
// It scans thirty-one columns into a positional dest list and then copies each
// local into a named Entry field. Nothing about that is type-checked: swapping
// two same-typed neighbours — status and nzo_id are both NullString, bytes and
// archive both NullInt64 — compiles, passes every query test that only checks
// a row count, and silently serves the wrong value through the history API.
//
// The columns asserted here are deliberately same-typed neighbours rather than
// a representative sample.
func TestScanEntry_MapsColumnsByPosition(t *testing.T) {
	got, err := scanEntry(fixedScanner{at: map[int]any{
		9:  "Failed",           // status
		10: "SABnzbd_nzo_xyz",  // nzo_id, the NullString next to it
		20: "par2 repair fail", // fail_message
		21: "http://info",      // url_info, its NullString neighbour
		22: int64(4242),        // bytes
		28: int64(1),           // archive, the NullInt64 next to it
		29: int64(1700000000),  // time_added
	}})
	if err != nil {
		t.Fatalf("scanEntry: %v", err)
	}
	if got.Status != "Failed" || got.NzoID != "SABnzbd_nzo_xyz" {
		t.Errorf("status/nzo_id = %q/%q, want Failed/SABnzbd_nzo_xyz — adjacent "+
			"NullString columns are transposed", got.Status, got.NzoID)
	}
	if got.FailMessage != "par2 repair fail" || got.URLInfo != "http://info" {
		t.Errorf("fail_message/url_info = %q/%q, transposed", got.FailMessage, got.URLInfo)
	}
	if got.Bytes != 4242 || got.Archive != 1 {
		t.Errorf("bytes/archive = %d/%d, want 4242/1 — adjacent NullInt64 columns "+
			"are transposed", got.Bytes, got.Archive)
	}
	if got.TimeAdded.Unix() != 1700000000 {
		t.Errorf("time_added = %v, want the unix value converted via fromUnix", got.TimeAdded)
	}
}

// TestScanEntry_PropagatesAScanError pins that a driver error is returned
// rather than yielding a half-filled Entry the caller would treat as real.
func TestScanEntry_PropagatesAScanError(t *testing.T) {
	boom := errors.New("column count mismatch")
	got, err := scanEntry(fixedScanner{err: boom})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the scan error", err)
	}
	if got != nil {
		t.Errorf("got a non-nil Entry alongside an error: %+v", got)
	}
}
