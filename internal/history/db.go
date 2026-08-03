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
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/pressly/goose/v3"

	// Register the pure-Go SQLite driver (no CGO required).
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

// DB wraps a SQLite connection pool configured for history access.
type DB struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path, applies the schema if
// the file is new, enables WAL mode and foreign keys, and runs VACUUM to
// reclaim free pages from prior deletes (spec §11.4).
//
// The returned *DB must be closed when the caller is done with it.
func Open(ctx context.Context, path string) (*DB, error) {
	// Connection-scoped pragmas MUST be in the DSN for modernc.org/sqlite
	// so they apply to every connection in the pool, not just one random
	// checkout. journal_mode=WAL is database-scoped (persists on disk)
	// and only needs to run once via Exec.
	// _txlock=immediate makes every BeginTx issue BEGIN IMMEDIATE, taking
	// the write lock up front instead of upgrading to it on the first write.
	//
	// Without it, a transaction that reads before it writes — Add's
	// SELECT MAX(sort_key) then INSERT is the canonical one — has to upgrade
	// mid-transaction, and SQLite answers a contended upgrade with
	// SQLITE_BUSY (or BUSY_SNAPSHOT) *immediately* rather than invoking the
	// busy handler: waiting while already holding a read snapshot could
	// deadlock. That put busy_timeout below out of reach for exactly the
	// case it was configured for, and made a job finalizing concurrently
	// with a job being added fail the add outright.
	//
	// Taking the lock at BEGIN removes the upgrade, so contention becomes an
	// ordinary wait that busy_timeout covers. The cost is that write
	// transactions serialize from their first statement rather than their
	// first write, which SQLite does anyway — it permits one writer at a
	// time regardless.
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: open %q: %w", path, err)
	}

	// Bound the pool — SQLite serializes writes anyway, and keeping
	// many idle connections just wastes file descriptors.
	sqlDB.SetMaxOpenConns(4)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close() // superseded by ping error
		return nil, fmt.Errorf("error pinging database: %s, %w", path, err)
	}

	// WAL mode is database-scoped (persists on disk) — only needs
	// to run once, not per-connection.
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		_ = sqlDB.Close() // superseded by open error
		return nil, fmt.Errorf("history: PRAGMA journal_mode=WAL: %w", err)
	}

	subFS, err := fs.Sub(embedMigrations, "migrations")
	if err != nil {
		_ = sqlDB.Close() // superseded by sub fs error
		return nil, fmt.Errorf("history: sub fs: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectSQLite3, sqlDB, subFS)
	if err != nil {
		_ = sqlDB.Close() // superseded by goose provider error
		return nil, fmt.Errorf("history: new goose provider: %w", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		_ = sqlDB.Close() // superseded by migration error
		return nil, fmt.Errorf("history: run migrations: %w", err)
	}

	if _, err := sqlDB.ExecContext(ctx, "VACUUM"); err != nil {
		_ = sqlDB.Close() // superseded by vacuum error
		return nil, fmt.Errorf("history: VACUUM: %w", err)
	}

	// 25 is deliberate headroom, not a measured figure: actual API
	// concurrency here is single-digit, and SQLite serializes writers
	// regardless of pool size (mitigated by busy_timeout(5000) in the
	// DSN above), so this will not realistically contend. Kept wide
	// rather than tuned to ~8-10 because there's no evidence tighter
	// bounds are needed; revisit if profiling shows connection
	// exhaustion or contention under real load. Accepted risk, tracked
	// as issue #112 (R6).
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(25)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	return &DB{db: sqlDB}, nil
}

// Close releases the underlying database connection pool. It is safe to call
// Close more than once; subsequent calls return nil.
func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("history: close: %w", err)
	}
	return nil
}

// Ping verifies that the underlying database connection is alive.
func (d *DB) Ping(ctx context.Context) error {
	if d == nil || d.db == nil {
		return errors.New("history: db is nil or closed")
	}
	if err := d.db.PingContext(ctx); err != nil {
		return fmt.Errorf("history: ping: %w", err)
	}
	return nil
}
