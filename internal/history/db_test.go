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
	t.Parallel()
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

	t.Run("Path reports the path Open was called with", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "history.db")
		db, err := Open(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("expected successful open, got: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if got := db.Path(); got != dbPath {
			t.Errorf("Path() = %q, want %q", got, dbPath)
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

func TestOpen_SetsSynchronousFullOnPooledConnections(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	db, err := Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	conn1, err := db.db.Conn(ctx)
	if err != nil {
		t.Fatalf("first Conn: %v", err)
	}
	t.Cleanup(func() { _ = conn1.Close() })

	// Check out a second connection while conn1 is still held so the pool
	// opens a distinct underlying SQLite connection, verifying the connection-scoped
	// DSN pragmas (synchronous, foreign_keys, busy_timeout) apply to every
	// pooled connection rather than only to the initial checkout at Open.
	conn2, err := db.db.Conn(ctx)
	if err != nil {
		t.Fatalf("second Conn: %v", err)
	}
	t.Cleanup(func() { _ = conn2.Close() })

	const (
		sqliteSynchronousFull = 2
		wantForeignKeys       = 1
		wantBusyTimeout       = 5000
	)
	for i, conn := range []*sql.Conn{conn1, conn2} {
		var syncMode int
		if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&syncMode); err != nil {
			t.Fatalf("conn %d PRAGMA synchronous: %v", i+1, err)
		}
		if syncMode != sqliteSynchronousFull {
			t.Errorf("conn %d PRAGMA synchronous = %d, want %d (FULL)", i+1, syncMode, sqliteSynchronousFull)
		}

		var foreignKeys int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("conn %d PRAGMA foreign_keys: %v", i+1, err)
		}
		if foreignKeys != wantForeignKeys {
			t.Errorf("conn %d PRAGMA foreign_keys = %d, want %d", i+1, foreignKeys, wantForeignKeys)
		}

		var busyTimeout int
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("conn %d PRAGMA busy_timeout: %v", i+1, err)
		}
		if busyTimeout != wantBusyTimeout {
			t.Errorf("conn %d PRAGMA busy_timeout = %d, want %d", i+1, busyTimeout, wantBusyTimeout)
		}
	}
}

func TestDB_Ping(t *testing.T) {
	t.Parallel()
	t.Run("nil db returns error", func(t *testing.T) {
		var db *DB
		if err := db.Ping(context.Background()); err == nil {
			t.Error("expected error for nil DB, got nil")
		}
	})

	t.Run("open db ping succeeds", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "history.db")
		db, err := Open(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close()

		if err := db.Ping(context.Background()); err != nil {
			t.Errorf("Ping on open db failed: %v", err)
		}
	})
}

func TestDB_Close_Nil(t *testing.T) {
	t.Parallel()
	var nilDB *DB
	if err := nilDB.Close(); err != nil {
		t.Errorf("expected nil error from nil DB Close, got: %v", err)
	}

	emptyDB := &DB{}
	if err := emptyDB.Close(); err != nil {
		t.Errorf("expected nil error from empty DB Close, got: %v", err)
	}
}
