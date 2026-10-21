package durability

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestStore_SeedRowsCountsWrittenArticlesPerFile pins the join: each file's
// count is its own written_articles rows (not the job's), a file with none
// reads 0, and a job with no job_files rows is absent from the map.
func TestStore_SeedRowsCountsWrittenArticlesPerFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewStore(openTestDB(t))

	if err := st.Admit(ctx, "job-a", []AdmitFile{
		{Subject: "a0", Bytes: 100, ArticleCount: 3, FetchPolicy: 0},
		{Subject: "a1", Bytes: 50, ArticleCount: 2, FetchPolicy: 2},
	}); err != nil {
		t.Fatalf("Admit a: %v", err)
	}
	if err := st.Admit(ctx, "job-b", []AdmitFile{
		{Subject: "b0", Bytes: 7, ArticleCount: 1, FetchPolicy: 1},
	}); err != nil {
		t.Fatalf("Admit b: %v", err)
	}
	if err := st.ApplyRecord(ctx, []RecordBatch{{
		JobID: "job-a",
		Rows: []WrittenRow{
			{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 10},
			{FileIdx: 0, ArtIdx: 1, Offset: 10, Length: 10},
			{FileIdx: 0, ArtIdx: 2, Offset: 20, Length: 10},
			{FileIdx: 1, ArtIdx: 0, Offset: 0, Length: 10},
			{FileIdx: 1, ArtIdx: 1, Offset: 10, Length: 10},
		},
		Files: []FileState{{FileIdx: 0, Complete: true, Filename: "a0.bin", FetchPolicy: 0}},
	}}); err != nil {
		t.Fatalf("ApplyRecord: %v", err)
	}

	got, unreadable, err := st.SeedRows(ctx)
	if err != nil || len(unreadable) != 0 {
		t.Fatalf("SeedRows: %v, unreadable %v", err, unreadable)
	}
	want := map[string][]SeedRow{
		"job-a": {
			{FileIndex: 0, Subject: "a0", Bytes: 100, ArticleCount: 3, Complete: true, FetchPolicy: 0, Filename: "a0.bin", Recorded: 3},
			{FileIndex: 1, Subject: "a1", Bytes: 50, ArticleCount: 2, FetchPolicy: 2, Recorded: 2},
		},
		"job-b": {
			{FileIndex: 0, Subject: "b0", Bytes: 7, ArticleCount: 1, FetchPolicy: 1, Recorded: 0},
		},
	}
	if len(got) != len(want) {
		t.Fatalf("SeedRows keys = %d, want %d: %+v", len(got), len(want), got)
	}
	for id, w := range want {
		if !slices.Equal(got[id], w) {
			t.Errorf("SeedRows[%s] = %+v, want %+v", id, got[id], w)
		}
	}
}

func TestStore_SeedRowsReportsAClosedDatabase(t *testing.T) {
	t.Parallel()
	if _, _, err := closedStore(t).SeedRows(context.Background()); err == nil {
		t.Fatal("SeedRows on a closed database returned nil error")
	}
}

// TestStore_SeedRowsIsolatesAJobWithAnUnreadableRow pins the blast radius: a
// row holding text where bytes belongs (SQLite's CHECK (bytes >= 0) accepts
// it, since text compares greater than any integer) costs its own job, which
// is dropped from rows and named in unreadable, and no other.
func TestStore_SeedRowsIsolatesAJobWithAnUnreadableRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestDB(t)
	st := NewStore(db)
	for _, id := range []string{"job-bad", "job-good"} {
		if err := st.Admit(ctx, id, []AdmitFile{
			{Subject: id + "0", Bytes: 10, ArticleCount: 1},
			{Subject: id + "1", Bytes: 20, ArticleCount: 2},
		}); err != nil {
			t.Fatalf("Admit %s: %v", id, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE job_files SET bytes = 'garbled' WHERE job_id = 'job-bad' AND file_index = 1`); err != nil {
		t.Fatalf("damage a row: %v", err)
	}

	got, unreadable, err := st.SeedRows(ctx)
	if err != nil {
		t.Fatalf("SeedRows: %v", err)
	}
	if _, ok := got["job-bad"]; ok {
		t.Errorf("the job with an unreadable row kept rows: %+v", got["job-bad"])
	}
	cause, ok := unreadable["job-bad"]
	if !ok {
		t.Fatalf("job-bad not named in unreadable: %v", unreadable)
	}
	if msg := cause.Error(); !strings.Contains(msg, "file index 1") || !strings.Contains(msg, "bytes") {
		t.Errorf("unreadable cause = %q, want the file index and column", msg)
	}
	if len(unreadable) != 1 {
		t.Errorf("unreadable = %v, want only job-bad", unreadable)
	}
	if len(got["job-good"]) != 2 {
		t.Errorf("the other job's rows = %+v, want both intact", got["job-good"])
	}
}
