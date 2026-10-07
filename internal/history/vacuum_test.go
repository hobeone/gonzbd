package history

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Open must not run a startup VACUUM. VACUUM rewrites the whole database
// file, so it needs the file to itself, costs disk proportional to database
// size, and used to turn a transient failure — such as no free space for
// its temporary copy — into a fatal Open error on every daemon start.
// SQLite reuses freed pages for later writes without it, and nothing this
// package does changes page_size or enables auto_vacuum, so there is no
// vacuum-dependent behavior to preserve.
//
// TestOpen_DoesNotVacuum below checks this directly, on the one signal a
// VACUUM and Open's writability check cannot produce alike: whether Open
// reclaims free pages. That used to be checked structurally instead, through
// TestOpen_WritabilityCheckIsBoundedByBusyTimeout's setup: since Open made no
// write at all once a database was already migrated and in WAL mode, it
// returned near-instant regardless of any concurrent writer, and a
// re-introduced VACUUM — needing an exclusive lock — would have queued behind
// one instead. That discriminator broke on purpose: Open's writability check
// (db.go, a PRAGMA user_version round-trip) is itself a write, so it now
// queues behind a held writer exactly as a VACUUM would have, and a test
// built on "which one blocks" can no longer tell them apart.
// TestOpen_WritabilityCheckIsBoundedByBusyTimeout keeps that setup for what
// it can still show: the write Open now makes is one ordinary, bounded one
// like any other in this package — it fails once busy_timeout elapses
// rather than hanging forever or silently succeeding past a writer it never
// actually reached. It no longer pins "no startup VACUUM" — it cannot tell
// one apart from Open's own write. TestOpen_DoesNotVacuum does that instead,
// below.
func TestOpen_WritabilityCheckIsBoundedByBusyTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "history.db")

	db1, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Hold an uncommitted write open on the same file from a second,
	// independent connection. Open's writability check has to queue behind
	// it.
	blocker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	t.Cleanup(func() { _ = blocker.Close() })
	tx, err := blocker.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin blocker tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(t.Context(), "CREATE TABLE _lock_holder(x INTEGER)"); err != nil {
		t.Fatalf("blocker write: %v", err)
	}

	start := time.Now()
	_, err = Open(t.Context(), path)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Open succeeded while a writer held the file locked the whole time; " +
			"want the writability check to fail once busy_timeout elapses")
	}
	if elapsed < 4*time.Second {
		t.Errorf("Open's writability check returned after %s; want it to wait out "+
			"busy_timeout(5000ms) rather than give up immediately", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Open's writability check took %s; want it bounded near "+
			"busy_timeout(5000ms), not unbounded", elapsed)
	}
}

// TestOpen_DoesNotVacuum is what pins "no startup VACUUM" now (see the
// package comment above). A VACUUM reclaims a database's freelist and
// shrinks its page count. Open's writability check does not, because the
// write it makes — a PRAGMA user_version header write on an
// already-migrated database — rewrites bytes already on the header page and
// allocates no B-tree pages. The claim below is scoped to that: a write
// which allocates pages, such as the INSERTs this test's own setup uses to
// create free pages in the first place, pulls pages from the freelist and
// would change these counts too; this test does not distinguish that case
// from a VACUUM. The database is left with free pages by adding and then
// deleting entries — deleting does not by itself prove anything, since a
// pruning DELETE runs in this package on every retention sweep without ever
// reclaiming space — and Open under test must leave both counts exactly as
// they were.
func TestOpen_DoesNotVacuum(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "history.db")

	db, repo := openTestDBAt(t, path)
	ctx := t.Context()

	const n = 300
	ids := make([]string, n)
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	for i := range n {
		id := fmt.Sprintf("vacuum-probe-%d", i)
		ids[i] = id
		e := sampleEntry(id, "name", "Completed", "movies")
		e.ScriptLog = make([]byte, 4096) // pad rows across enough pages to free some on delete
		if err := repo.AddTx(ctx, tx, e); err != nil {
			t.Fatalf("AddTx %s: %v", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := repo.Delete(ctx, ids...); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	pageCountBefore := pragmaInt(t, db.db, "page_count")
	freelistBefore := pragmaInt(t, db.db, "freelist_count")
	if freelistBefore == 0 {
		t.Fatal("setup did not produce any free pages; the property under test is not exercised")
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	pageCountAfter := pragmaInt(t, db2.db, "page_count")
	freelistAfter := pragmaInt(t, db2.db, "freelist_count")

	if pageCountAfter != pageCountBefore {
		t.Errorf("page_count = %d after Open, want unchanged %d — a header write that allocates no pages must not change it",
			pageCountAfter, pageCountBefore)
	}
	if freelistAfter != freelistBefore {
		t.Errorf("freelist_count = %d after Open, want unchanged %d — a header write that allocates no pages must not reclaim free pages",
			freelistAfter, freelistBefore)
	}
}

// openTestDBAt is openTestDB with a caller-chosen path, needed where the test
// re-Opens the same file rather than letting t.TempDir() pick a fresh one.
func openTestDBAt(t *testing.T, path string) (*DB, *Repository) {
	t.Helper()
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db, NewRepository(db)
}

// pragmaInt reads a single-column integer PRAGMA.
func pragmaInt(t *testing.T, db *sql.DB, pragma string) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", pragma, err)
	}
	return v
}

// TestOpenClose_MultipleRounds verifies that Open+Close can be called
// repeatedly without error, confirming migrations are idempotent.
func TestOpenClose_MultipleRounds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "history.db")

	for i := range 3 {
		db, err := Open(t.Context(), path)
		if err != nil {
			t.Fatalf("Open round %d: %v", i, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close round %d: %v", i, err)
		}
	}
}
