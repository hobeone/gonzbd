package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"
)

// openSeedTestDB returns a migrated history database for the seed tests.
func openSeedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return history.NewRepository(db).DB()
}

// countSeeded returns how many job_files rows exist for one job.
func countSeeded(t *testing.T, db *sql.DB, jobID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM job_files WHERE job_id = ?`, jobID,
	).Scan(&n); err != nil {
		t.Fatalf("count job_files: %v", err)
	}
	return n
}

// TestSeedJobFiles_OneRowPerFile pins the shape of the seed: exactly one row
// per file, at the file's own index, holding the empty results the checkpointer
// later fills in.
//
// The indices are asserted individually rather than only counted because a seed
// that wrote N rows at the wrong indices would satisfy a count and still leave
// every SaveBatch UPDATE matching nothing — the silent failure the seed exists
// to prevent.
func TestSeedJobFiles_OneRowPerFile(t *testing.T) {
	db := openSeedTestDB(t)

	if err := seedJobFiles(t.Context(), db, "job-a", 4); err != nil {
		t.Fatalf("seedJobFiles: %v", err)
	}

	rows, err := db.Query(
		`SELECT file_index, complete, fetch_policy, filename, assembled_crc32
		   FROM job_files WHERE job_id = ? ORDER BY file_index`, "job-a")
	if err != nil {
		t.Fatalf("query job_files: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var got []int
	for rows.Next() {
		var fi, complete, policy, crc int
		var filename string
		if err := rows.Scan(&fi, &complete, &policy, &filename, &crc); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if complete != 0 || policy != 0 || filename != "" || crc != 0 {
			t.Errorf("file %d seeded with results already set: complete=%d policy=%d filename=%q crc=%d",
				fi, complete, policy, filename, crc)
		}
		got = append(got, fi)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	want := []int{0, 1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("seeded indices = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seeded indices = %v, want %v", got, want)
		}
	}
}

// TestSeedJobFiles_IsIdempotent pins the ON CONFLICT DO NOTHING clause. A job
// re-added under the same ID must not fail, and must not disturb results the
// checkpointer has already written — DO NOTHING rather than an upsert, because
// an upsert would reset a completed file's filename and CRC back to empty.
func TestSeedJobFiles_IsIdempotent(t *testing.T) {
	db := openSeedTestDB(t)

	if err := seedJobFiles(t.Context(), db, "job-b", 3); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE job_files SET complete = 1, filename = 'a.rar' WHERE job_id = ? AND file_index = 1`,
		"job-b",
	); err != nil {
		t.Fatalf("simulate checkpoint: %v", err)
	}
	if err := seedJobFiles(t.Context(), db, "job-b", 3); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	if n := countSeeded(t, db, "job-b"); n != 3 {
		t.Errorf("job_files rows after re-seed = %d, want 3", n)
	}
	var complete int
	var filename string
	if err := db.QueryRow(
		`SELECT complete, filename FROM job_files WHERE job_id = ? AND file_index = 1`, "job-b",
	).Scan(&complete, &filename); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if complete != 1 || filename != "a.rar" {
		t.Errorf("re-seed clobbered checkpointed results: complete=%d filename=%q, want 1 and \"a.rar\"",
			complete, filename)
	}
}

// TestSeedJobFiles_IsAllOrNothing pins the transaction. A failure partway
// through must leave NO rows, not a prefix of them.
//
// The prefix is the dangerous outcome rather than the empty result: by the time
// the seed runs the job is already registered with the dispatcher and about to
// download, and SaveBatch's UPDATE matching no row is not an error, so the
// unseeded tail would persist no metadata at all and hydrate with defaults —
// with nothing logged anywhere.
//
// The fault is injected with a trigger rather than a production seam: the seed
// takes a *sql.DB and has no injection point, and a constraint that fires
// inside SQLite is a truer stand-in for the real failures here (a disk error, a
// busy timeout) than a Go-side hook would be.
func TestSeedJobFiles_IsAllOrNothing(t *testing.T) {
	db := openSeedTestDB(t)

	if _, err := db.Exec(`
CREATE TRIGGER fail_on_index_three BEFORE INSERT ON job_files
WHEN NEW.file_index = 3
BEGIN SELECT RAISE(ABORT, 'injected fault'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	err := seedJobFiles(t.Context(), db, "job-c", 6)
	if err == nil {
		t.Fatal("seedJobFiles returned nil, want the injected fault reported")
	}
	if n := countSeeded(t, db, "job-c"); n != 0 {
		t.Errorf("job_files rows after a failed seed = %d, want 0 — files 0..2 committed "+
			"before the fault, so the tail of this job would silently persist no metadata", n)
	}
}

// TestSeedJobFiles_ZeroFilesCommits pins that a manifest with no files is not
// an error. The loop body never runs, so this reaches Commit with an empty
// transaction, which is the one path the other three tests never take.
func TestSeedJobFiles_ZeroFilesCommits(t *testing.T) {
	db := openSeedTestDB(t)

	if err := seedJobFiles(t.Context(), db, "job-d", 0); err != nil {
		t.Fatalf("seedJobFiles with no files: %v", err)
	}
	if n := countSeeded(t, db, "job-d"); n != 0 {
		t.Errorf("job_files rows = %d, want 0", n)
	}
}

// TestSeedJobFiles_CancelledContextSeedsNothing pins the BeginTx error path.
func TestSeedJobFiles_CancelledContextSeedsNothing(t *testing.T) {
	db := openSeedTestDB(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := seedJobFiles(ctx, db, "job-e", 3); err == nil {
		t.Fatal("seedJobFiles returned nil for a cancelled context, want an error")
	}
	if n := countSeeded(t, db, "job-e"); n != 0 {
		t.Errorf("job_files rows = %d, want 0", n)
	}
}
