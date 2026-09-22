package durability

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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
func ruleStatement(t perJobTable, filtered bool) string {
	q := `DELETE FROM ` + t.name + `
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = ` + t.name + `.job_id)`
	if t.keptForFailedEntry {
		q += `
   AND NOT EXISTS (SELECT 1 FROM history h WHERE h.nzo_id = ` + t.name + `.job_id AND h.status = ?)`
	}
	if filtered {
		// One JSON parameter, not one placeholder per id, so a large history
		// delete cannot exceed SQLite's host-parameter limit.
		q += `
   AND job_id IN (SELECT value FROM json_each(?))`
	}
	return q
}

func ruleArgs(t perJobTable, ids []byte) []any {
	var args []any
	if t.keptForFailedEntry {
		args = append(args, string(constants.StatusFailed))
	}
	if ids != nil {
		args = append(args, string(ids))
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
	ids, err := json.Marshal(append([]string{id}, more...))
	if err != nil {
		return fmt.Errorf("durability: reclaim: encode ids: %w", err)
	}
	return s.inTx(ctx, "reclaim", func(tx *sql.Tx) error { return applyRule(ctx, tx, ids) })
}

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
// is non-nil.
func applyRule(ctx context.Context, tx *sql.Tx, ids []byte) error {
	for _, t := range perJobTables {
		if _, err := tx.ExecContext(ctx, ruleStatement(t, ids != nil), ruleArgs(t, ids)...); err != nil {
			return fmt.Errorf("durability: reclaim %s: %w", t.name, err)
		}
	}
	return nil
}
