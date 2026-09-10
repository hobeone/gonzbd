// Package history manages the gonzbd download history database (history.db).
// It provides a thin SQLite-backed store whose `history` table descends from
// the upstream Python implementation's and has since diverged from it.
//
// It is NOT interchangeable with an upstream history.db, and this package is
// what makes that so: Open fails on one. Standing Design Rule 1 is the reason
// — gonzbd targets fresh installations and is not a drop-in replacement — and
// the divergences are listed in docs/sabnzbd_spec.md §11.2.
//
// Which mechanism refuses it depends on the file, and it is worth being exact
// because the obvious summary is wrong. refuseUnknownSchema rejects a database
// recording a goose version above what this build ships. An upstream file
// records none at all — it has no goose_db_version table — so that guard
// passes it through, and it is the migration itself that fails, on CREATE
// TABLE history against a file that already has one.
//
// This comment used to claim the schema was "byte-for-byte compatible with the
// upstream Python implementation, so users can run the Go daemon against an
// existing history file without a migration step". That was contradicted by
// code in this same file, and the claim is what kept three unread columns
// alive on the strength of a migration path that cannot happen.
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

	// Refuse a database recording a migration version ABOVE the highest this
	// build ships, BEFORE running the migrations.
	//
	// The schema is one migration, and has twice been collapsed back to one
	// after a chain grew. goose keys purely on version numbers, so a database
	// left at a version the collapse discarded would read as already migrated:
	// Up() applies nothing and returns nil against a schema missing whatever
	// the discarded chain built.
	//
	// That failure is silent by construction, and it has been observed. The
	// daemon came up clean with no durability tables at all: every barrier
	// failed on its commit with a plain error rather than a
	// *storagefault.Fault, so checkpointJob logged one Warn and did not stall,
	// nothing was ever acked, no job completed, and the only signal was a
	// last_barrier_unix that never advanced.
	//
	// Checked before Up rather than by looking for the tables afterwards,
	// because this names the cause: the operator is told their database
	// predates the collapse, which is actionable, rather than that a table is
	// missing, which is not.
	//
	// WHAT IT DOES NOT CATCH, and why that is accepted rather than unnoticed.
	// A version number is not a schema identity, and 001_initial.sql is
	// rewritten in place on each collapse, so "version 1" names several
	// materially different schemas across this repository's history. Two cases
	// slip past:
	//
	//   - A database recording exactly version 1, written by a build whose 001
	//     was an earlier file. `git log --oneline -- migrations/001_initial.sql`
	//     shows six such revisions; a database created between the 2026-08-15
	//     rebuild and 2026-08-23, when 002 was added, records version 1 and
	//     nothing else. It is accepted here, Up() applies nothing, and the
	//     daemon runs against that older schema. Deliberately not defended:
	//     Standing Design Rule 1 puts an eight-day-old installation out of
	//     scope, and the alternatives — renumbering every collapse past all
	//     previously used versions, or probing for a sentinel table — buy a
	//     case that cannot arise here at the cost of a rule that has to be
	//     re-derived by every later reader.
	//
	//   - An upstream SABnzbd history.db, which has no goose_db_version table
	//     at all and so leaves via the not-migrated branch below. It is still
	//     refused, but by Up() failing on `CREATE TABLE history` against a file
	//     that already has that table, not by this guard. The message is worse
	//     and there is no upgrade either way.
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
	// This is the pool's only bound (#302): an earlier commit also capped it
	// at 4 immediately after sql.Open, on the reasoning that a narrow pool
	// suits the startup work above (ping, WAL, migrations, VACUUM). That
	// reasoning does not hold: startup runs sequentially on one connection
	// regardless of the cap, so the 4-wide bound never had any effect to
	// begin with and has been deleted rather than kept as a second bound.
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
// It is not a "downgrade" guard in the usual sense. The schema is a single
// migration that has twice absorbed a chain grown on top of it, and goose keys
// purely on version numbers — so a database left at a version the collapse
// discarded records migrations that no longer exist, while version 1 reads as
// already applied and Up() does nothing at all. The failure is silent by
// construction, which is why it is refused rather than repaired.
//
// It is a bound in ONE direction. See Open for the two cases a version
// comparison cannot reach — a legacy database recording exactly version 1, and
// an upstream file recording no version at all.
var ErrSchemaFromTheFuture = errors.New("history: the database records migrations this build does not have")

// refuseUnknownSchema fails when goose_db_version holds a version above the
// highest migration this build embeds. It is not a schema check: it compares
// one integer, and everything it cannot see is enumerated on Open.
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
		// Ordinarily "no such table": nothing has ever been migrated here,
		// which is the fresh-install path and by far the common case.
		//
		// This deliberately does not distinguish that from the other reasons
		// the read can fail — a corrupt or differently-shaped goose_db_version,
		// a busy database — and so fails OPEN on all of them. The cost is
		// bounded: every such database reaches provider.Up next, which either
		// migrates it or fails loudly, so the outcome is a worse error message
		// rather than a silent success. Narrowing this to the "no such table"
		// case would trade that for a second place to keep a driver's error
		// vocabulary correct.
		return nil //nolint:nilerr // a fresh database is the ordinary case, not a failure
	}
	if !applied.Valid || applied.Int64 <= highest {
		return nil
	}
	return fmt.Errorf(
		"%w: it is at version %d and this build ships up to %d. The migration chain was "+
			"collapsed into 001 and there is no upgrade path from a pre-collapse database, "+
			"so this database cannot be read: move it aside and let gonzbd create a new one",
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
