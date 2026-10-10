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
	t.Parallel()
	ctx := context.Background()
	db := openTestDB(t)
	st := NewStore(db)

	if err := st.Admit(ctx, "job-a", []uint8{0, 2}); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := st.ApplyRecord(ctx, []RecordBatch{{JobID: "job-a", Files: []FileState{
		{FileIdx: 0, Complete: true, FetchPolicy: 1, Filename: "a.bin"},
	}}}); err != nil {
		t.Fatalf("ApplyRecord: %v", err)
	}
	if err := st.Admit(ctx, "job-a", []uint8{0, 2, 1}); err != nil {
		t.Fatalf("re-Admit: %v", err)
	}

	rows, err := st.FileRows(ctx, "job-a")
	if err != nil {
		t.Fatalf("FileRows: %v", err)
	}
	want := []FileRow{
		{FileIndex: 0, Complete: true, FetchPolicy: 1, Filename: "a.bin"},
		{FileIndex: 1, FetchPolicy: 2},
		{FileIndex: 2, FetchPolicy: 1},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("rows after a re-seed = %+v, want %+v — the seed overwrote retained progress, "+
			"or failed to add the new file", rows, want)
	}
}

// TestStore_FileRowsSkipsARowItCannotScan pins the partial read: the bad row
// is skipped, the others are returned, and the error says the result is
// incomplete rather than failed.
func TestStore_FileRowsSkipsARowItCannotScan(t *testing.T) {
	t.Parallel()
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

// TestStore_ProgressMethodsReportAClosedDatabase covers every method's first
// failure path: none may swallow an error its caller has to act on.
func TestStore_ProgressMethodsReportAClosedDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := closedStore(t)
	calls := map[string]func() error{
		"Admit":    func() error { return st.Admit(ctx, "j", []uint8{0}) },
		"FileRows": func() error { _, err := st.FileRows(ctx, "j"); return err },
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
