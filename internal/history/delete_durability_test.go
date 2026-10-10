package history

import (
	"context"
	"testing"
)

// TestDelete_LeavesTheJobsDurabilityRows pins where Delete's ownership ends. A
// job's job_files and written_articles rows are not history's:
// durability.Store.Reclaim decides them after the entry is gone, and a Delete
// that took them would be a second deleter deciding the same rows by a
// different rule.
func TestDelete_LeavesTheJobsDurabilityRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, repo := openTestDB(t)

	if err := repo.Add(ctx, Entry{NzoID: "job-1", Name: "job-1", Status: "Failed"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO job_files (job_id, file_index) VALUES ('job-1',0)`,
		`INSERT INTO written_articles (job_id, file_idx, art_idx, offset, length, crc32)
		 VALUES ('job-1',0,0,0,100,7)`,
	} {
		if _, err := db.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := db.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE job_id = ?", "job-1").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if n, err := repo.Delete(ctx, "job-1"); err != nil || n != 1 {
		t.Fatalf("Delete = %d, %v; want 1, nil", n, err)
	}
	for _, table := range []string{"job_files", "written_articles"} {
		if n := count(table); n != 1 {
			t.Errorf("%d %s rows after Delete, want 1 — they are reclaimed by "+
				"durability's rule, not by history", n, table)
		}
	}

	// An empty ID list opens no transaction and reports nothing deleted.
	if n, err := repo.Delete(ctx); err != nil || n != 0 {
		t.Errorf("Delete() with no IDs = %d, %v; want 0, nil", n, err)
	}
}
