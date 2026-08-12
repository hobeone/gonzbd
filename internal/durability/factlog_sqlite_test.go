package durability

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"

	// Register the pure-Go SQLite driver (no CGO required).
	_ "modernc.org/sqlite"
)

// openTestDB opens a scratch SQLite database under t.TempDir() with the
// article_facts schema applied, and returns the raw *sql.DB.
//
// history.Open runs the embedded goose migrations, but its returned *DB
// wraps the connection in an unexported field with no schema accessor, and
// embedMigrations is unexported to this package (embed patterns cannot
// cross ".." out of internal/history's directory). So this opens the file
// once through history.Open to apply the migrations, closes that handle,
// and reopens the same file directly to hand the test a raw *sql.DB.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.db")
	hdb, err := history.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	if err := hdb.Close(); err != nil {
		t.Fatalf("close migration handle: %v", err)
	}

	dsn := path + "?_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close scratch db: %v", err)
		}
	})
	return db
}

func TestSQLiteFactLog_AppendIsIdempotent(t *testing.T) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openTestDB(t))

	fact := ArticleFact{FileIdx: 0, ArtIdx: 5, Offset: 1024, Length: 768000, CRC32: 0xDEADBEEF}
	if err := fl.Append(ctx, "job-1", []ArticleFact{fact}); err != nil {
		t.Fatal(err)
	}
	// A re-delivered article must not error and must not change the record.
	if err := fl.Append(ctx, "job-1", []ArticleFact{fact}); err != nil {
		t.Fatalf("second Append: %v", err)
	}

	got, err := fl.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ForFile returned %d facts, want 1", len(got))
	}
	if got[0] != fact {
		t.Errorf("ForFile = %+v, want %+v", got[0], fact)
	}
}

// TestSQLiteFactLog_AppendNeverUpdates pins R1: a Class A fact is immutable.
// A hostile or buggy second delivery reporting a different offset must not
// overwrite the first — the file's bytes were written against the original.
func TestSQLiteFactLog_AppendNeverUpdates(t *testing.T) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openTestDB(t))

	orig := ArticleFact{FileIdx: 0, ArtIdx: 5, Offset: 1024, Length: 100, CRC32: 1}
	evil := ArticleFact{FileIdx: 0, ArtIdx: 5, Offset: 999999, Length: 100, CRC32: 2}
	if err := fl.Append(ctx, "job-1", []ArticleFact{orig}); err != nil {
		t.Fatal(err)
	}
	if err := fl.Append(ctx, "job-1", []ArticleFact{evil}); err != nil {
		t.Fatal(err)
	}
	got, _ := fl.ForFile(ctx, "job-1", 0)
	if got[0].Offset != 1024 {
		t.Fatalf("Offset = %d, want 1024 — a Class A fact was mutated", got[0].Offset)
	}
}

func TestSQLiteFactLog_ForFileIsOrderedByOffset(t *testing.T) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openTestDB(t))
	// art_idx ascends as offset DESCENDS. That inversion is the whole point:
	// article_facts is WITHOUT ROWID keyed on (job_id, art_idx), so an
	// unordered scan returns primary-key order. A fixture whose art_idx and
	// offset both ascend produces the same sequence either way, and the test
	// passes with ORDER BY deleted — it pins nothing. Task 6's gaplessPrefix
	// walks this result assuming offset order, so the clause is load-bearing.
	facts := []ArticleFact{
		{FileIdx: 0, ArtIdx: 0, Offset: 2000, Length: 500},
		{FileIdx: 0, ArtIdx: 2, Offset: 0, Length: 500},
		{FileIdx: 1, ArtIdx: 3, Offset: 0, Length: 500},
		{FileIdx: 0, ArtIdx: 1, Offset: 1000, Length: 500},
	}
	if err := fl.Append(ctx, "job-1", facts); err != nil {
		t.Fatal(err)
	}
	got, err := fl.ForFile(ctx, "job-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ForFile(0) returned %d, want 3 (file 1 must not leak in)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Offset <= got[i-1].Offset {
			t.Fatalf("ForFile not ordered by offset: %v", got)
		}
	}
}

func TestSQLiteFactLog_DeleteJobIsScoped(t *testing.T) {
	ctx := context.Background()
	fl := NewSQLiteFactLog(openTestDB(t))
	f := ArticleFact{FileIdx: 0, ArtIdx: 1, Offset: 0, Length: 10}
	if err := fl.Append(ctx, "job-1", []ArticleFact{f}); err != nil {
		t.Fatal(err)
	}
	if err := fl.Append(ctx, "job-2", []ArticleFact{f}); err != nil {
		t.Fatal(err)
	}
	if err := fl.DeleteJob(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := fl.ForFile(ctx, "job-1", 0); len(got) != 0 {
		t.Errorf("job-1 has %d facts after DeleteJob, want 0", len(got))
	}
	if got, _ := fl.ForFile(ctx, "job-2", 0); len(got) != 1 {
		t.Errorf("DeleteJob(job-1) removed job-2's facts")
	}
}

// TestSQLiteFactLog_AppendEmptyIsNoop covers Append's early return: an empty
// batch must not touch the database at all, so it succeeds even against
// an already-closed handle that would fail any real query.
func TestSQLiteFactLog_AppendEmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	fl := NewSQLiteFactLog(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := fl.Append(ctx, "job-1", nil); err != nil {
		t.Fatalf("Append(nil) on a closed db = %v, want nil (should be a no-op)", err)
	}
}

// TestSQLiteFactLog_AppendErrorsOnClosedDB covers the BeginTx error path:
// a non-empty batch against a closed handle must return a wrapped error,
// not panic or silently drop the facts.
func TestSQLiteFactLog_AppendErrorsOnClosedDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	fl := NewSQLiteFactLog(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	fact := ArticleFact{FileIdx: 0, ArtIdx: 1, Offset: 0, Length: 10}
	if err := fl.Append(ctx, "job-1", []ArticleFact{fact}); err == nil {
		t.Fatal("Append on a closed db = nil error, want an error")
	}
}

// TestSQLiteFactLog_DeleteJobErrorsOnClosedDB covers DeleteJob's error path.
func TestSQLiteFactLog_DeleteJobErrorsOnClosedDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	fl := NewSQLiteFactLog(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := fl.DeleteJob(ctx, "job-1"); err == nil {
		t.Fatal("DeleteJob on a closed db = nil error, want an error")
	}
}
