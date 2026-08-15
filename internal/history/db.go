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
	"strconv"
	"strings"
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
	//
	// This is not the steady-state value: it is raised to 25 at the end of
	// this function, so 4 governs only the startup below (ping, WAL,
	// migrations, VACUUM). See #302 — the two bounds arrived in separate
	// commits and it is unresolved whether the staging is deliberate.
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

	// Refuse a database this build's migrations cannot describe, BEFORE
	// running them.
	//
	// The 001-011 chain was collapsed into one migration, so goose sees a
	// pre-existing database as already at version 1 and Up() applies nothing
	// and returns nil. The daemon then came up clean with no article_facts and
	// no file_extents: every barrier failed in priorExtent with a plain error
	// rather than a *storagefault.Fault, so checkpointJob logged one Warn and
	// did not stall, nothing was ever acked, no job completed, and the only
	// signal was a last_barrier_unix that never advanced.
	//
	// Checked before Up rather than by looking for the tables afterwards,
	// because this names the cause: the operator is told their database
	// predates the collapse, which is actionable, rather than that a table is
	// missing, which is not.
	if err := refuseUnknownSchema(ctx, sqlDB, subFS); err != nil {
		_ = sqlDB.Close() // superseded by schema error
		return nil, err
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
	// concurrency here is single-digit, and SQLite permits one writer at a
	// time regardless of pool size, so a wider pool buys queueing rather
	// than parallelism. Kept wide rather than tuned to ~8-10 because there
	// is no evidence tighter bounds are needed; revisit if profiling shows
	// connection exhaustion or contention under real load.
	//
	// This used to argue that contention was "mitigated by busy_timeout(5000)
	// in the DSN above", and record the residual as an accepted risk under
	// #112. Both parts have gone stale. #112 is closed, and the mitigation
	// had a hole: busy_timeout only governs waits the busy handler runs, and
	// a deferred transaction upgrading from read to write never reached it —
	// SQLite fails a contended upgrade immediately rather than risk a
	// deadlock. That is what made concurrent adds fail outright, and it is
	// fixed above by _txlock=immediate, which takes the write lock at BEGIN
	// so contention is an ordinary wait that busy_timeout does cover.
	//
	// Note the pool is bounded twice in this function: 4 near the top, which
	// governs startup through the migrations and VACUUM, and 25 here for
	// everything after. Whether that staging is intended is unresolved —
	// see #302.
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

// ErrSchemaFromTheFuture reports a database recording migrations this build
// does not ship.
//
// It is not a "downgrade" guard in the usual sense. The 001-011 chain was
// collapsed into a single 001, and goose keys purely on version numbers — so a
// database written by any earlier build records versions 2..11 that no longer
// exist, and Up() sees version 1 as already applied and does nothing at all.
// The failure is silent by construction, which is why it is refused rather
// than repaired.
var ErrSchemaFromTheFuture = errors.New("history: the database records migrations this build does not have")

// refuseUnknownSchema fails when goose_db_version holds a version above the
// highest migration this build embeds.
//
// A missing table is not an error: a fresh database has no goose_db_version
// until the first migration runs, which is the ordinary first start.
func refuseUnknownSchema(ctx context.Context, db *sql.DB, migrations fs.FS) error {
	highest, err := highestEmbeddedVersion(migrations)
	if err != nil {
		return err
	}
	var applied sql.NullInt64
	row := db.QueryRowContext(ctx, `SELECT MAX(version_id) FROM goose_db_version`)
	if err := row.Scan(&applied); err != nil {
		// No such table: nothing has ever been migrated here.
		return nil //nolint:nilerr // a fresh database is the ordinary case, not a failure
	}
	if !applied.Valid || applied.Int64 <= highest {
		return nil
	}
	return fmt.Errorf(
		"%w: it is at version %d and this build ships up to %d. The 001-011 chain was "+
			"collapsed into a single migration and there is no upgrade path, so this "+
			"database cannot be read: move it aside and let gonzbd create a new one",
		ErrSchemaFromTheFuture, applied.Int64, highest)
}

// highestEmbeddedVersion reads the largest migration number this build ships.
func highestEmbeddedVersion(migrations fs.FS) (int64, error) {
	entries, err := fs.ReadDir(migrations, ".")
	if err != nil {
		return 0, fmt.Errorf("history: read migrations: %w", err)
	}
	var highest int64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, ok := strings.Cut(name, "_")
		if !ok {
			continue
		}
		v, convErr := strconv.ParseInt(num, 10, 64)
		if convErr != nil {
			continue
		}
		highest = max(highest, v)
	}
	if highest == 0 {
		return 0, fmt.Errorf("history: no numbered migrations are embedded")
	}
	return highest, nil
}
