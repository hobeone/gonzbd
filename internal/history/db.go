// Package history manages the gonzbd download history database (history.db).
// It provides a thin SQLite-backed store whose schema is byte-for-byte
// compatible with the upstream Python implementation, so users can run the Go
// daemon against an existing history file without a migration step.
//
// Concurrency model: a single *DB value is safe for concurrent use. All
// exported methods on Repository accept a context.Context; callers may cancel
// or time-out individual operations without affecting others.
package history

import (
	"database/sql"
	"embed"
	"fmt"
	"sync"
	"time"

	"github.com/pressly/goose/v3"

	// Register the pure-Go SQLite driver (no CGO required).
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

var initGooseErr = sync.OnceValue(func() error {
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("history: set goose dialect: %w", err)
	}
	return nil
})

// DB wraps a SQLite connection pool configured for history access.
type DB struct {
	db              *sql.DB
	connMaxLifetime time.Duration
	maxOpenConns    int
	maxIdleConns    int
}

// Open opens (or creates) the SQLite database at path, applies the schema if
// the file is new, enables WAL mode and foreign keys, and runs VACUUM to
// reclaim free pages from prior deletes (spec §11.4).
//
// The returned *DB must be closed when the caller is done with it.
func Open(path string) (*DB, error) {
	// Connection-scoped pragmas MUST be in the DSN for modernc.org/sqlite
	// so they apply to every connection in the pool, not just one random
	// checkout. journal_mode=WAL is database-scoped (persists on disk)
	// and only needs to run once via Exec.
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: open %q: %w", path, err)
	}

	// Bound the pool — SQLite serializes writes anyway, and keeping
	// many idle connections just wastes file descriptors.
	sqlDB.SetMaxOpenConns(4)

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close() //nolint:errcheck // superseded by ping error
		return nil, fmt.Errorf("error pinging database: %s, %w", path, err)
	}

	// WAL mode is database-scoped (persists on disk) — only needs
	// to run once, not per-connection.
	if _, err := sqlDB.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = sqlDB.Close() //nolint:errcheck // superseded by open error
		return nil, fmt.Errorf("history: PRAGMA journal_mode=WAL: %w", err)
	}

	if err := initGooseErr(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("history: run migrations: %w", err)
	}

	if _, err := sqlDB.Exec("VACUUM"); err != nil {
		_ = sqlDB.Close() //nolint:errcheck // superseded by vacuum error
		return nil, fmt.Errorf("history: VACUUM: %w", err)
	}

	maxOpenConns := 25
	maxIdleConns := 25
	connMaxLifetime := 5 * time.Minute

	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxIdleConns)
	sqlDB.SetConnMaxLifetime(connMaxLifetime)

	return &DB{
		db:              sqlDB,
		connMaxLifetime: connMaxLifetime,
		maxOpenConns:    maxOpenConns,
		maxIdleConns:    maxIdleConns,
	}, nil
}

// Close releases the underlying database connection pool. It is safe to call
// Close more than once; subsequent calls return the same error as the first.
func (d *DB) Close() error {
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("history: close: %w", err)
	}
	return nil
}
