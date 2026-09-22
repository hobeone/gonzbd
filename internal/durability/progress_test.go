package durability

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
)

func closedStore(t *testing.T) *Store {
	t.Helper()
	db := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return NewStore(db)
}

func countRows(t *testing.T, db *sql.DB, table, jobID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestStore_AdmitSeedsAndKeepsExistingRows pins both halves of the seed: a row
// per file at its fetch policy with empty results, and ON CONFLICT DO NOTHING,
// which is what lets a retry re-seed a job without erasing retained progress.
func TestStore_AdmitSeedsAndKeepsExistingRows(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	st := NewStore(db)

	if err := st.Admit(ctx, "job-a", []uint8{0, 2}); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := st.SaveProgress(ctx, []JobProgress{{JobID: "job-a", Files: []FileRow{
		{FileIndex: 0, Complete: true, FetchPolicy: 1, Filename: "a.bin", AssembledCRC32: 9},
	}}}); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	if err := st.Admit(ctx, "job-a", []uint8{0, 2, 1}); err != nil {
		t.Fatalf("re-Admit: %v", err)
	}

	rows, err := st.FileRows(ctx, "job-a")
	if err != nil {
		t.Fatalf("FileRows: %v", err)
	}
	want := []FileRow{
		{FileIndex: 0, Complete: true, FetchPolicy: 1, Filename: "a.bin", AssembledCRC32: 9},
		{FileIndex: 1, FetchPolicy: 2},
		{FileIndex: 2, FetchPolicy: 1},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("rows after a re-seed = %+v, want %+v — the seed overwrote retained progress, "+
			"or failed to add the new file", rows, want)
	}
}

// TestStore_SaveProgressUpdatesFilesAndAddsFailedMarks pins that a checkpoint
// updates seeded rows and only ever ADDS failed marks: a second batch without
// an article does not clear the mark the first one wrote.
func TestStore_SaveProgressUpdatesFilesAndAddsFailedMarks(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	st := NewStore(db)
	if err := st.Admit(ctx, "job-a", []uint8{0}); err != nil {
		t.Fatal(err)
	}

	for _, failed := range [][]int{{3, 5}, {5}} {
		if err := st.SaveProgress(ctx, []JobProgress{{
			JobID:          "job-a",
			Files:          []FileRow{{FileIndex: 0, Filename: "x", FetchPolicy: 2}},
			FailedArticles: failed,
		}}); err != nil {
			t.Fatalf("SaveProgress: %v", err)
		}
	}

	got, err := st.FailedArticles(ctx, "job-a")
	if err != nil {
		t.Fatalf("FailedArticles: %v", err)
	}
	if !slices.Equal(got, []int32{3, 5}) {
		t.Errorf("failed articles = %v, want [3 5]", got)
	}
	rows, _ := st.FileRows(ctx, "job-a")
	if len(rows) != 1 || rows[0].Filename != "x" || rows[0].FetchPolicy != 2 {
		t.Errorf("file row = %+v, want the checkpointed filename and policy", rows)
	}
	if err := st.SaveProgress(ctx, nil); err != nil {
		t.Errorf("empty batch = %v, want nil", err)
	}
}

// TestStore_FileRowsSkipsARowItCannotScan pins the partial read: the bad row
// is skipped, the others are returned, and the error says the result is
// incomplete rather than failed.
func TestStore_FileRowsSkipsARowItCannotScan(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	st := NewStore(db)
	if err := st.Admit(ctx, "job-a", []uint8{0, 0}); err != nil {
		t.Fatal(err)
	}
	// filename is nullable, and NULL does not scan into a string.
	if _, err := db.Exec(`UPDATE job_files SET filename = NULL WHERE job_id = 'job-a' AND file_index = 0`); err != nil {
		t.Fatal(err)
	}

	rows, err := st.FileRows(ctx, "job-a")
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want it to wrap ErrIncomplete", err)
	}
	if len(rows) != 1 || rows[0].FileIndex != 1 {
		t.Errorf("rows = %+v, want file 1 alone — one bad row must not cost the others", rows)
	}
}

// TestStore_FailedArticlesStopsAtARowItCannotScan pins the other read's
// partial contract: what was read before the bad row comes back, marked
// incomplete.
func TestStore_FailedArticlesStopsAtARowItCannotScan(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	st := NewStore(db)
	// ORDER BY art_idx sorts the integer before the text value, so 1 is read first.
	if _, err := db.Exec(`INSERT INTO failed_articles (job_id, art_idx) VALUES ('job-a', 1), ('job-a', 'x')`); err != nil {
		t.Fatal(err)
	}

	got, err := st.FailedArticles(ctx, "job-a")
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want it to wrap ErrIncomplete", err)
	}
	if !slices.Equal(got, []int32{1}) {
		t.Errorf("got %v, want [1]: the index read before the bad row", got)
	}
}

// TestStore_DiscardsAreScopedToOneJob pins that each discard removes one
// table's rows for one job and nothing else.
func TestStore_DiscardsAreScopedToOneJob(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	st := NewStore(db)
	for _, id := range []string{"job-a", "job-b"} {
		if err := st.Admit(ctx, id, []uint8{0}); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveProgress(ctx, []JobProgress{{JobID: id, FailedArticles: []int{4}}}); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.DiscardFileRows(ctx, "job-a"); err != nil {
		t.Fatalf("DiscardFileRows: %v", err)
	}
	if n := countRows(t, db, "job_files", "job-a"); n != 0 {
		t.Errorf("job-a has %d job_files rows after the discard", n)
	}
	if n := countRows(t, db, "failed_articles", "job-a"); n != 1 {
		t.Errorf("job-a has %d failed_articles rows, want 1 — DiscardFileRows reached another table", n)
	}

	if err := st.DiscardFailedArticles(ctx, "job-a"); err != nil {
		t.Fatalf("DiscardFailedArticles: %v", err)
	}
	if n := countRows(t, db, "failed_articles", "job-a"); n != 0 {
		t.Errorf("job-a has %d failed_articles rows after the discard", n)
	}
	for _, table := range []string{"job_files", "failed_articles"} {
		if n := countRows(t, db, table, "job-b"); n != 1 {
			t.Errorf("job-b has %d %s rows, want 1 — a discard was not scoped to its job", n, table)
		}
	}
}

// TestStore_ProgressMethodsReportAClosedDatabase covers every method's first
// failure path: none may swallow an error its caller has to act on.
func TestStore_ProgressMethodsReportAClosedDatabase(t *testing.T) {
	ctx := context.Background()
	st := closedStore(t)
	calls := map[string]func() error{
		"Admit":                 func() error { return st.Admit(ctx, "j", []uint8{0}) },
		"SaveProgress":          func() error { return st.SaveProgress(ctx, []JobProgress{{JobID: "j"}}) },
		"DiscardFileRows":       func() error { return st.DiscardFileRows(ctx, "j") },
		"DiscardFailedArticles": func() error { return st.DiscardFailedArticles(ctx, "j") },
		"FileRows":              func() error { _, err := st.FileRows(ctx, "j"); return err },
		"FailedArticles":        func() error { _, err := st.FailedArticles(ctx, "j"); return err },
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s on a closed database = nil, want an error", name)
		}
		if errors.Is(err, ErrIncomplete) {
			t.Errorf("%s: a failed query claims to be a partial read: %v", name, err)
		}
	}
}
