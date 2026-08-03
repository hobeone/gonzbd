package history

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// T5: Open() with invalid or corrupt path returns meaningful errors.
func TestOpen_InvalidPath(t *testing.T) {
	t.Parallel()

	t.Run("nonexistent directory", func(t *testing.T) {
		t.Parallel()
		_, err := Open(t.Context(), "/nonexistent/deep/path/history.db")
		if err == nil {
			t.Fatal("expected error for nonexistent directory")
		}
	})

	t.Run("path is a directory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Try to open the directory itself as a database file.
		_, err := Open(t.Context(), dir)
		if err == nil {
			t.Fatal("expected error when path is a directory")
		}
	})
}

// T6: allColumns column count matches scanEntry's Scan argument count.
// This guards against drift between the SELECT column list and the
// scanEntry function — a common source of "sql: expected N destination
// arguments, got M" panics.
func TestAllColumns_CountMatchesScanEntry(t *testing.T) {
	t.Parallel()

	// Count columns in allColumns by splitting on commas.
	colCount := len(strings.Split(allColumns, ","))

	// Open a real DB, insert a row, and attempt a SELECT to verify
	// the column list matches scanEntry's Scan call.
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	repo := NewRepository(db)
	ctx := context.Background()

	// Insert a minimal entry.
	entry := Entry{
		NzoID:  "test-nzo-id",
		Name:   "test-name",
		Status: "Completed",
	}
	if err := repo.Add(ctx, entry); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Get should succeed if allColumns and scanEntry are in sync.
	got, err := repo.Get(ctx, "test-nzo-id")
	if err != nil {
		t.Fatalf("Get failed (column count mismatch?): %v", err)
	}
	if got.Name != "test-name" {
		t.Errorf("Name = %q, want %q", got.Name, "test-name")
	}

	// The allColumns constant has 31 columns. Verify this as a safety net.
	if colCount != 31 {
		t.Errorf("allColumns has %d columns, expected 31 — did you add/remove a column without updating scanEntry?", colCount)
	}
}

func TestOpen_CancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "cancelled.db")
	_, err := Open(ctx, path)
	if err == nil {
		t.Fatal("expected error with cancelled context, got nil")
	}
}
