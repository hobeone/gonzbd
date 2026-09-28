package history

import (
	"database/sql"
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
// TestOpen_DoesNotBlockBehindAWriter checks this structurally rather than by
// reading the source: a VACUUM needs an exclusive lock, so if Open ran one
// it would queue behind a writer holding an uncommitted write open
// elsewhere on the same file, and wait out Open's own busy_timeout(5000ms)
// before failing. Open must instead return promptly regardless of that
// writer.
func TestOpen_DoesNotBlockBehindAWriter(t *testing.T) {
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
	// independent connection. A VACUUM issued by the Open under test would
	// have to wait behind it.
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
	db2, err := Open(t.Context(), path)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Open with a concurrent writer held open: %v (after %s) — "+
			"a startup VACUUM would block behind that writer and fail once "+
			"Open's own busy_timeout(5000ms) elapses", err, elapsed)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("Open took %s with a concurrent writer held open; want near-instant — "+
			"this magnitude of delay is what a startup VACUUM queued behind that writer "+
			"would produce", elapsed)
	}
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
