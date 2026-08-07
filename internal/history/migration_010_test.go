package history

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// migrationProviderAt opens a scratch database and returns a goose provider
// over the same embedded migrations Open uses.
//
// This test lives in package history rather than package queue, where the
// columns are read, because Open runs provider.Up straight to head and
// exposes no way to stop at a version. Driving goose to 009, seeding, and
// then applying 010 needs the provider itself, and embedMigrations is
// package-level here. Reaching it from another package would mean adding a
// cross-package testing API for one test.
func migrationProviderAt(t *testing.T) (*goose.Provider, *sql.DB) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "migration.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	subFS, err := fs.Sub(embedMigrations, "migrations")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, subFS)
	if err != nil {
		t.Fatalf("new goose provider: %v", err)
	}
	return provider, db
}

// hasColumn reports whether table has a column of the given name.
func hasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns of %s: %v", table, err)
	}
	return false
}

// TestMigration010_BackfillsRecoveryFromJobFiles pins the migration that
// replaces the jobs table's par2_bytes/par2_files with recovery_bytes and
// recovery_files.
//
// The backfill is what this test is really about. #318 records that landing
// this work requires a full reset, so a migration that left zeros would be
// defensible — but a zero recovery figure is not a missing reading. It is a
// definite claim that the job has no repair capacity, which the UI renders as
// "No repair data" and both abort gates read as grounds to declare a job
// hopeless. The backfill is exact (the figures are precisely the
// is_par2_recovery aggregate), so leaving that claim to chance would be a
// choice, not a saving.
func TestMigration010_BackfillsRecoveryFromJobFiles(t *testing.T) {
	t.Parallel()
	provider, db := migrationProviderAt(t)
	ctx := context.Background()

	if _, err := provider.UpTo(ctx, 9); err != nil {
		t.Fatalf("migrate to 009: %v", err)
	}

	// Guard the premise: at 009 the old columns exist and the new ones do
	// not. Without this the assertions below could pass against a schema
	// that never had the old shape.
	if !hasColumn(t, db, "jobs", "par2_bytes") {
		t.Fatal("fixture guard: jobs.par2_bytes missing at migration 009, so this test is not exercising the replacement")
	}
	if hasColumn(t, db, "jobs", "recovery_bytes") {
		t.Fatal("fixture guard: jobs.recovery_bytes already exists at 009")
	}

	// A job whose par2 set is an index plus two recovery volumes. par2_bytes
	// is seeded with the pre-migration meaning — index included — so the
	// backfill has something wrong to correct rather than merely copy.
	const jobID = "job-with-index"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO jobs (id, filename, name, priority, status, pp, time_added, md5, avg_age, sort_key, par2_bytes, par2_files)
		 VALUES (?, 't.nzb', 't', 0, 'Queued', 3, 0, '', 0, 0, ?, ?)`,
		jobID, 750, 3); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	files := []struct {
		idx        int
		bytes      int64
		isRecovery int
	}{
		{0, 1000, 0}, // content
		{1, 50, 0},   // par2 index: not a recovery volume
		{2, 300, 1},
		{3, 400, 1},
	}
	for _, f := range files {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO job_files (job_id, file_index, subject, date, bytes, article_count, is_par2_recovery)
			 VALUES (?, ?, 's', 0, ?, 1, ?)`,
			jobID, f.idx, f.bytes, f.isRecovery); err != nil {
			t.Fatalf("seed job_files[%d]: %v", f.idx, err)
		}
	}

	if _, err := provider.UpTo(ctx, 10); err != nil {
		t.Fatalf("migrate to 010: %v", err)
	}

	if hasColumn(t, db, "jobs", "par2_bytes") || hasColumn(t, db, "jobs", "par2_files") {
		t.Error("jobs still has par2_bytes/par2_files after 010")
	}

	var gotBytes int64
	var gotFiles int
	if err := db.QueryRowContext(ctx,
		"SELECT recovery_bytes, recovery_files FROM jobs WHERE id = ?", jobID,
	).Scan(&gotBytes, &gotFiles); err != nil {
		t.Fatalf("read recovery columns: %v", err)
	}
	if gotBytes != 700 {
		t.Errorf("recovery_bytes = %d, want 700 (the two volumes only; 750 would mean the 50 B index is still counted)", gotBytes)
	}
	if gotFiles != 2 {
		t.Errorf("recovery_files = %d, want 2 (volumes only; 3 would mean the index is still counted)", gotFiles)
	}
}

// TestMigration010_JobWithoutFilesBackfillsToZero pins the documented limit of
// the backfill. addTx writes job_files rows only when a manifest is in hand,
// so a job without rows has nothing to aggregate and lands on zero. That is
// acceptable only because #318 requires a full reset; it is recorded here so
// the gap is a known property rather than a surprise.
func TestMigration010_JobWithoutFilesBackfillsToZero(t *testing.T) {
	t.Parallel()
	provider, db := migrationProviderAt(t)
	ctx := context.Background()

	if _, err := provider.UpTo(ctx, 9); err != nil {
		t.Fatalf("migrate to 009: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO jobs (id, filename, name, priority, status, pp, time_added, md5, avg_age, sort_key, par2_bytes, par2_files)
		 VALUES ('no-files', 't.nzb', 't', 0, 'Queued', 3, 0, '', 0, 0, 500, 2)`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := provider.UpTo(ctx, 10); err != nil {
		t.Fatalf("migrate to 010: %v", err)
	}

	var gotBytes int64
	if err := db.QueryRowContext(ctx,
		"SELECT recovery_bytes FROM jobs WHERE id = 'no-files'").Scan(&gotBytes); err != nil {
		t.Fatalf("read recovery_bytes: %v", err)
	}
	if gotBytes != 0 {
		t.Errorf("recovery_bytes = %d, want 0 — a job with no job_files rows has nothing to aggregate", gotBytes)
	}
}

// TestMigration010_DownRestoresSchema pins that the reversal is applicable.
// It restores the schema but not the values: the par2 index's bytes are not
// recoverable from job_files, which flags recovery volumes only, so a
// down-migrated row carries the recovery figures rather than the originals.
func TestMigration010_DownRestoresSchema(t *testing.T) {
	t.Parallel()
	provider, db := migrationProviderAt(t)
	ctx := context.Background()

	if _, err := provider.UpTo(ctx, 10); err != nil {
		t.Fatalf("migrate to 010: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO jobs (id, filename, name, priority, status, pp, time_added, md5, avg_age, sort_key, recovery_bytes, recovery_files)
		 VALUES ('down', 't.nzb', 't', 0, 'Queued', 3, 0, '', 0, 0, 700, 2)`); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("migrate down from 010: %v", err)
	}

	if !hasColumn(t, db, "jobs", "par2_bytes") || !hasColumn(t, db, "jobs", "par2_files") {
		t.Fatal("Down did not restore par2_bytes/par2_files")
	}
	if hasColumn(t, db, "jobs", "recovery_bytes") {
		t.Error("Down left recovery_bytes in place")
	}

	var gotBytes int64
	if err := db.QueryRowContext(ctx,
		"SELECT par2_bytes FROM jobs WHERE id = 'down'").Scan(&gotBytes); err != nil {
		t.Fatalf("read par2_bytes: %v", err)
	}
	// 700, not the 750 an index-inclusive figure would have held. The loss is
	// the point being pinned, not an oversight.
	if gotBytes != 700 {
		t.Errorf("par2_bytes = %d after Down, want 700 (the recovery figure carried across; the index's bytes are unrecoverable)", gotBytes)
	}
}
