package history

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpen(t *testing.T) {
	t.Run("successful open and close", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "history.db")
		db, err := Open(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("expected successful open, got: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("expected successful close, got: %v", err)
		}
	})

	t.Run("ping error when directory path is provided as db file", func(t *testing.T) {
		dir := t.TempDir()
		// Passing a directory path as the sqlite file path causes PingContext to fail.
		_, err := Open(context.Background(), dir)
		if err == nil {
			t.Fatal("expected error opening directory as database file, got nil")
		}
	})

	t.Run("wal pragma error when directory is read-only", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "readonly_wal.db")
		// Create the file first so PingContext succeeds when opening the file for reading.
		f, err := os.Create(dbPath)
		if err != nil {
			t.Fatalf("failed to create temp db file: %v", err)
		}
		f.Close()

		// Make the directory read-only so SQLite cannot create -wal and -shm files.
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatalf("failed to chmod dir: %v", err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(dir, 0700)
		})

		_, err = Open(context.Background(), dbPath)
		if err == nil {
			t.Fatal("expected error setting WAL pragma in read-only directory, got nil")
		}
	})

	t.Run("migration error when table already exists without goose metadata", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "conflict.db")

		// Pre-create a conflicting history table directly via database/sql
		// without running goose migrations.
		sqlDB, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("failed to open raw sqlite db: %v", err)
		}
		_, err = sqlDB.Exec("CREATE TABLE history (conflict_column TEXT);")
		if err != nil {
			t.Fatalf("failed to create conflicting table: %v", err)
		}
		sqlDB.Close()

		// Open via history.Open should fail during goose migration Up().
		_, err = Open(context.Background(), dbPath)
		if err == nil {
			t.Fatal("expected migration error due to conflicting table, got nil")
		}
	})
}
