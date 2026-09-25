package history

import (
	"context"
	"testing"
)

// TestDelete_TakesRetainedProgressAndLeavesDurabilityRows pins where Delete's
// ownership ends. The entry's retained per-file progress goes with it, in the
// same transaction, because history owns both. A job's durable_runs and
// failed_articles are not history's: durability.Store.Reclaim decides them
// after the entry is gone, and a Delete that took them would be a second
// deleter deciding the same rows by a different rule.
func TestDelete_TakesRetainedProgressAndLeavesDurabilityRows(t *testing.T) {
	ctx := context.Background()
	db, repo := openTestDB(t)

	if err := repo.Add(ctx, Entry{NzoID: "job-1", Name: "job-1", Status: "Failed"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO durable_runs (job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32)
		 VALUES ('job-1',0,0,0,0,100,7)`,
		`INSERT INTO failed_articles (job_id, art_idx) VALUES ('job-1',1)`,
		`INSERT INTO history_job_files (job_id, file_index, complete, article_count)
		 VALUES ('job-1',0,0,2)`,
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
	if n := count("history_job_files"); n != 0 {
		t.Errorf("%d history_job_files rows survive their entry's deletion", n)
	}
	for _, table := range []string{"durable_runs", "failed_articles"} {
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
