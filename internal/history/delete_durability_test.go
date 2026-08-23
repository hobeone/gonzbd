package history

import (
	"context"
	"testing"
)

// TestDelete_DropDurabilityIsTheOnlyDifferenceBetweenTheTwoEntryPoints pins
// the shared implementation directly, against both values of the flag its two
// callers exist to set.
//
// Delete and DeleteKeepingDurability differ in exactly one bit, and the whole
// argument for splitting them into two names rather than one bool parameter is
// that the decision belongs in the type rather than at each call site. That
// argument is only worth anything if the bit does what the names claim, and
// neither exported test can see the two side by side: they call one entry
// point each.
//
// The retained rows are what a retry uses to bound its truncate to the whole
// partial file rather than to the articles it re-fetched — #422 was
// RetryHistoryJob calling straight through the dropping path and destroying
// exactly them.
func TestDelete_DropDurabilityIsTheOnlyDifferenceBetweenTheTwoEntryPoints(t *testing.T) {
	ctx := context.Background()
	db, repo := openTestDB(t)

	seed := func(id string) {
		t.Helper()
		if err := repo.Add(ctx, Entry{NzoID: id, Name: id, Status: "Failed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO durable_runs (job_id, file_idx, first_art_idx, last_art_idx, offset, length, crc32)
			VALUES (?,0,0,0,0,100,7)`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.ExecContext(ctx,
			`INSERT INTO failed_articles (job_id, art_idx) VALUES (?,1)`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO history_job_files (job_id, file_index, complete, article_count)
			VALUES (?,0,0,2)`, id); err != nil {
			t.Fatal(err)
		}
	}
	count := func(table, id string) int {
		t.Helper()
		var n int
		if err := db.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE job_id = ?", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	seed("job-drop")
	seed("job-keep")

	if n, err := repo.delete(ctx, true, "job-drop"); err != nil || n != 1 {
		t.Fatalf("delete(drop) = %d, %v; want 1, nil", n, err)
	}
	if n, err := repo.delete(ctx, false, "job-keep"); err != nil || n != 1 {
		t.Fatalf("delete(keep) = %d, %v; want 1, nil", n, err)
	}

	for _, table := range []string{"durable_runs", "failed_articles"} {
		if n := count(table, "job-drop"); n != 0 {
			t.Errorf("%d %s rows survive a dropping delete; nothing else removes them, so "+
				"they accumulate one set per failed job forever", n, table)
		}
		if n := count(table, "job-keep"); n != 1 {
			t.Errorf("%d %s rows after a KEEPING delete, want 1 — the retry that follows "+
				"needs them to bound its truncate to the whole partial file (#422)", n, table)
		}
	}
	// The retained per-file progress is NOT part of the difference: both
	// entry points take it, because it is owned by the history entry and has
	// no foreign key to cascade from.
	for _, id := range []string{"job-drop", "job-keep"} {
		if n := count("history_job_files", id); n != 0 {
			t.Errorf("%d history_job_files rows survive %s's deletion; the retained progress "+
				"is owned by the entry and goes with it either way", n, id)
		}
	}

	// An empty ID list opens no transaction and reports nothing deleted.
	if n, err := repo.delete(ctx, true); err != nil || n != 0 {
		t.Errorf("delete() with no IDs = %d, %v; want 0, nil", n, err)
	}
}
