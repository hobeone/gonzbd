package durability

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/hobeone/gonzbd/internal/constants"
)

// perJobTable is one table Store owns whose rows belong to a single job,
// keyed by job_id.
type perJobTable struct {
	name string
	// keptForFailedEntry marks the one table whose rows outlive the queue row
	// while the job is a FAILED history entry: a retry reads a failed job's
	// durable_runs to bound FinalizeFile's truncate to the whole partial file
	// (#422). job_files and failed_articles are not read by a retry.
	keptForFailedEntry bool
}

// perJobTables is every table the reclaim rule covers.
// TestPerJobTables_CoversEveryJobKeyedTable fails when the schema gains a
// job_id table this list does not name.
var perJobTables = []perJobTable{
	{name: "job_files"},
	{name: "failed_articles"},
	{name: "durable_runs", keptForFailedEntry: true},
}

// The reclaim rule, as SQL: a job's rows go when nothing reaches the job — no
// queue row, and no FAILED history entry for the one table a failed entry
// keeps. ruleStatement is its only text. Reclaim and SweepOrphans differ only
// in the id filter appended to it, so they cannot disagree about the rule.
//
// NOT EXISTS rather than NOT IN: NOT IN over a subquery that yields a NULL is
// never true, so a NULL id would reclaim nothing, silently.
func ruleStatement(t perJobTable, n int) string {
	q := `DELETE FROM ` + t.name + `
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = ` + t.name + `.job_id)`
	if t.keptForFailedEntry {
		q += `
   AND NOT EXISTS (SELECT 1 FROM history h WHERE h.nzo_id = ` + t.name + `.job_id AND h.status = ?)`
	}
	if n > 0 {
		// One placeholder per id, so SQLite plans a plain index search on
		// job_id. Callers chunk to stay under its host-parameter limit.
		q += `
   AND job_id IN (` + strings.TrimSuffix(strings.Repeat("?,", n), ",") + `)`
	}
	return q
}

func ruleArgs(t perJobTable, ids []string) []any {
	args := make([]any, 0, len(ids)+1)
	if t.keptForFailedEntry {
		args = append(args, string(constants.StatusFailed))
	}
	for _, id := range ids {
		args = append(args, id)
	}
	return args
}

// Reclaim applies the reclaim rule to the named jobs, in one transaction. It
// is idempotent and reads only the database's current state, so a caller may
// run it after a departure that succeeded, failed, or was made by someone
// else: it never removes a row something still reaches.
//
// At least one id is required by the signature, so a caller cannot reach the
// every-job form by passing nothing; that form is SweepOrphans.
func (s *Store) Reclaim(ctx context.Context, id string, more ...string) error {
	ids := append([]string{id}, more...)
	return s.inTx(ctx, "reclaim", func(tx *sql.Tx) error {
		// Chunked so a bulk history delete cannot exceed SQLite's
		// host-parameter limit; one transaction covers every chunk.
		for chunk := range slices.Chunk(ids, reclaimChunk) {
			if err := applyRule(ctx, tx, chunk); err != nil {
				return err
			}
		}
		return nil
	})
}

// reclaimChunk bounds how many ids one statement names. SQLite's default
// SQLITE_MAX_VARIABLE_NUMBER is 32766; this leaves room for the status
// parameter and any future clause.
const reclaimChunk = 500

// SweepOrphans applies the reclaim rule to every job, in one transaction.
//
// Startup only, before anything can call Admit: Admit seeds job_files before
// the job's queue row exists, and the rule would reclaim a job in that window.
func (s *Store) SweepOrphans(ctx context.Context) error {
	return s.inTx(ctx, "sweep orphans", func(tx *sql.Tx) error { return applyRule(ctx, tx, nil) })
}

func (s *Store) inTx(ctx context.Context, op string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("durability: %s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("durability: %s: commit: %w", op, err)
	}
	return nil
}

// applyRule runs the rule over every per-job table, filtered to ids when ids
// is non-empty.
func applyRule(ctx context.Context, tx *sql.Tx, ids []string) error {
	for _, t := range perJobTables {
		if _, err := tx.ExecContext(ctx, ruleStatement(t, len(ids)), ruleArgs(t, ids)...); err != nil {
			return fmt.Errorf("durability: reclaim %s: %w", t.name, err)
		}
	}
	return nil
}
