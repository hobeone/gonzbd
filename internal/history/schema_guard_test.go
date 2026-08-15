package history

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

// TestOpen_RefusesADatabaseFromBeforeTheMigrationCollapse pins the loudest
// possible failure for the one upgrade that cannot work.
//
// The 001-011 chain was collapsed into a single migration, and goose keys on
// version numbers alone. A database written by any earlier build records
// versions 2..11 that no longer exist, and goose sees version 1 as already
// applied — so Up() applies nothing and returns nil.
//
// The daemon then came up CLEAN with no article_facts and no file_extents.
// Every barrier failed in priorExtent with a plain error rather than a
// *storagefault.Fault, so checkpointJob logged one Warn and did not stall.
// Nothing was ever acked, no job ever completed, and the only signal was a
// last_barrier_unix that never advanced — which the barrier stamps anyway when
// a job has no open files.
func TestOpen_RefusesADatabaseFromBeforeTheMigrationCollapse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-collapse.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE goose_db_version (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		version_id INTEGER NOT NULL,
		is_applied INTEGER NOT NULL,
		tstamp TIMESTAMP DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatal(err)
	}
	for v := range 12 { // 0..11, as the chain that was collapsed left it
		if _, err := db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	opened, err := Open(context.Background(), path)
	if err == nil {
		_ = opened.Close()
		t.Fatal("Open succeeded against a pre-collapse database. goose applies nothing, " +
			"so the daemon runs with no article_facts and no file_extents: every barrier " +
			"fails with a plain error that does not stall, nothing is ever acked, and no " +
			"job ever completes")
	}
	if !errors.Is(err, ErrSchemaFromTheFuture) {
		t.Errorf("err = %v, want ErrSchemaFromTheFuture", err)
	}
}

// TestOpen_AcceptsAFreshDatabaseAndItsOwnSchema is the half that keeps the
// guard above from being satisfied by refusing everything.
func TestOpen_AcceptsAFreshDatabaseAndItsOwnSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")

	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("a fresh database was refused: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// And re-opening the database this build just wrote, which records exactly
	// the version it ships.
	again, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("a database this build created was refused on re-open: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestHighestEmbeddedVersion keeps the bound honest. Read from the embedded
// files rather than hard-coded, so adding a migration cannot leave the guard
// refusing the database that migration produces.
func TestHighestEmbeddedVersion(t *testing.T) {
	sub, err := fs.Sub(embedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	got, err := highestEmbeddedVersion(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got < 1 {
		t.Fatalf("highestEmbeddedVersion = %d; with no numbered migration the guard "+
			"would refuse every database, including a fresh one", got)
	}
}

// TestRefuseUnknownSchema covers the branches Open cannot reach, and the
// error paths that would otherwise fail open.
func TestRefuseUnknownSchema(t *testing.T) {
	sub, err := fs.Sub(embedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}

	newDB := func(t *testing.T) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}

	t.Run("no goose table is a fresh database", func(t *testing.T) {
		if err := refuseUnknownSchema(context.Background(), newDB(t), sub); err != nil {
			t.Errorf("a database with no migration history was refused: %v", err)
		}
	})

	t.Run("an empty goose table is not a version", func(t *testing.T) {
		db := newDB(t)
		if _, err := db.Exec(`CREATE TABLE goose_db_version (version_id INTEGER)`); err != nil {
			t.Fatal(err)
		}
		// MAX over no rows is NULL, which is neither a version nor a failure.
		if err := refuseUnknownSchema(context.Background(), db, sub); err != nil {
			t.Errorf("a database with an empty migration table was refused: %v", err)
		}
	})

	t.Run("a version this build ships is accepted", func(t *testing.T) {
		db := newDB(t)
		if _, err := db.Exec(`CREATE TABLE goose_db_version (version_id INTEGER)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO goose_db_version VALUES (1)`); err != nil {
			t.Fatal(err)
		}
		if err := refuseUnknownSchema(context.Background(), db, sub); err != nil {
			t.Errorf("a database at this build's own version was refused: %v", err)
		}
	})

	t.Run("a version beyond this build is refused", func(t *testing.T) {
		db := newDB(t)
		if _, err := db.Exec(`CREATE TABLE goose_db_version (version_id INTEGER)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO goose_db_version VALUES (11)`); err != nil {
			t.Fatal(err)
		}
		err := refuseUnknownSchema(context.Background(), db, sub)
		if !errors.Is(err, ErrSchemaFromTheFuture) {
			t.Errorf("err = %v, want ErrSchemaFromTheFuture", err)
		}
	})
}

// TestHighestEmbeddedVersion_RefusesAnEmptySet keeps the guard from failing
// open. With no numbered migration the bound would be zero, every database
// would look like it came from the future, and a fresh install would be
// refused — so the absence is an error rather than a bound of zero.
func TestHighestEmbeddedVersion_RefusesAnEmptySet(t *testing.T) {
	if _, err := highestEmbeddedVersion(fstest.MapFS{
		"README.md":      &fstest.MapFile{},
		"notanumber.sql": &fstest.MapFile{},
	}); err == nil {
		t.Error("a migrations directory with no numbered file produced a bound rather " +
			"than an error; the guard would then refuse every database, fresh ones included")
	}
	if _, err := highestEmbeddedVersion(fstest.MapFS{}); err == nil {
		t.Error("an empty migrations directory produced a bound rather than an error")
	}
	got, err := highestEmbeddedVersion(fstest.MapFS{
		"001_a.sql": &fstest.MapFile{},
		"007_b.sql": &fstest.MapFile{},
		"003_c.sql": &fstest.MapFile{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Errorf("highestEmbeddedVersion = %d, want 7 — the bound is the HIGHEST "+
			"migration, not the last one read", got)
	}
}
