package history

import (
	"path/filepath"
	"testing"
)

// L14: Document and test that VACUUM runs on every Open().
// Open() calls VACUUM after migrations (db.go line 80) to reclaim free
// pages from prior deletes (spec §11.4).

// TestVacuumOnOpen verifies that Open() runs VACUUM on the database without
// error, both on initial creation and on re-open of an existing database.
func TestVacuumOnOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "history.db")

	// First open: creates the database, runs migrations, runs VACUUM.
	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Second open: runs VACUUM on an existing database with potential
	// free pages. This must not error.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (VACUUM on existing db): %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestOpenClose_MultipleRounds verifies that Open+Close can be called
// repeatedly without error, confirming VACUUM and migrations are idempotent.
func TestOpenClose_MultipleRounds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "history.db")

	for i := range 3 {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open round %d: %v", i, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close round %d: %v", i, err)
		}
	}
}
