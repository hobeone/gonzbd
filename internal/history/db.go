// Package history manages the SABnzbd download history database (history1.db).
// It provides a thin SQLite-backed store whose schema is byte-for-byte
// compatible with the upstream Python implementation, so users can run the Go
// daemon against an existing history file without a migration step.
//
// Concurrency model: a single *DB value is safe for concurrent use. All
// exported methods on Repository accept a context.Context; callers may cancel
// or time-out individual operations without affecting others.
package history

import (
	"embed"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/pressly/goose/v3"

	// Register the pure-Go SQLite driver (no CGO required).
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

var gooseOnce sync.Once

func initGoose() {
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		// SetDialect only fails if the dialect is unsupported.
		panic(fmt.Sprintf("history: failed to set goose dialect: %v", err))
	}
}

// DB wraps a SQLite connection pool configured for history access.
type DB struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path, applies the schema if
// the file is new, enables WAL mode and foreign keys, and runs VACUUM to
// reclaim free pages from prior deletes (spec §11.4).
//
// The returned *DB must be closed when the caller is done with it.
func Open(path string) (*DB, error) {
	// The modernc driver name is "sqlite".
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("history: open %q: %w", path, err)
	}

	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("error pinging database: %s, %w", path, err)
	}

	// WAL mode allows concurrent readers alongside the writer; foreign keys
	// are off by default in SQLite and must be enabled per-connection.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"pragma busy_timeout(5000)",
		"pragma synchronous(NORMAL)",
		"pragma foreign_keys(ON)",
	} {
		if _, err := sqlDB.Exec(pragma); err != nil {
			_ = sqlDB.Close() //nolint:errcheck // superseded by open error
			return nil, fmt.Errorf("history: %s: %w", pragma, err)
		}
	}

	gooseOnce.Do(initGoose)
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("history: run migrations: %w", err)
	}

	if _, err := sqlDB.Exec("VACUUM"); err != nil {
		_ = sqlDB.Close() //nolint:errcheck // superseded by vacuum error
		return nil, fmt.Errorf("history: VACUUM: %w", err)
	}

	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(25)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	return &DB{db: sqlDB}, nil
}

// Close releases the underlying database connection pool. It is safe to call
// Close more than once; subsequent calls return the same error as the first.
func (d *DB) Close() error {
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("history: close: %w", err)
	}
	return nil
}
