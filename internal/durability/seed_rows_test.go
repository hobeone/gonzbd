package durability

import (
	"context"
	"slices"
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

	got, err := st.SeedRows(ctx)
	if err != nil {
		t.Fatalf("SeedRows: %v", err)
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
	if _, err := closedStore(t).SeedRows(context.Background()); err == nil {
		t.Fatal("SeedRows on a closed database returned nil error")
	}
}
