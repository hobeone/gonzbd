package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hobeone/gonzbd/internal/dispatch"
)

// Store persists the dispatcher's queue in the dispatch_jobs table.
type Store struct {
	db  *sql.DB
	log *slog.Logger
}

var _ dispatch.Store = (*Store)(nil)

// New returns a Store over db. The caller owns db and its lifetime; Store
// neither opens nor closes it. If log is nil, a discard logger is used.
func New(db *sql.DB, log *slog.Logger) *Store {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{
		db:  db,
		log: log.With("component", "dispatch_store"),
	}
}

// columns is the column list Load scans and Save writes, in one place so the
// two cannot drift into disagreeing about order. A mismatch between them would
// not fail to compile — every value is an integer or a string — it would
// silently transpose fields.
const columns = `id, sort_key, name, category, priority, bytes,
	verify, repair, unpack, delete_ok,
	state, next, activity, outcome, assessed, intent,
	filename, ingest_anomaly, fail_reason, duplicate_reason, operational_error, script, password, pp, nzb_backup, url, md5,
	added, download_started, download_finished, par2_release_reason,
	recovery_bytes, par2_recovered, unwanted_ext`

// Load returns every stored job in queue order.
//
// The ID tiebreak makes the result total rather than merely mostly-ordered:
// SQLite is free to return rows sharing a sort_key in any order, and
// dispatch.restore sorts with the same tiebreak so the two agree.
//
// A row that fails rows.Scan (for example, an out-of-range integer in a narrow
// enum column or a NULL in a non-nullable Go field) is logged at Error and
// skipped so a single corrupt row cannot prevent the rest of the queue from
// loading. A failure of the query or cursor iteration itself is returned as an
// error.
func (s *Store) Load(ctx context.Context) ([]dispatch.Persisted, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+columns+` FROM dispatch_jobs ORDER BY sort_key ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("dispatch/store: load: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []dispatch.Persisted
	for rows.Next() {
		// Scanned straight into the destination fields. database/sql converts
		// an INTEGER column to a *bool, and narrows into a named uint8 type
		// (job.State and friends) with its own range check — a 256 in the
		// state column fails the scan with `converting driver.Value type
		// int64 ("256") to a uint8: value out of range` rather than
		// truncating to 0, which is StateUnset and would read as a legal
		// never-run job.
		//
		// That check is why there is no hand-written narrowing helper here.
		// An earlier revision scanned into ten intermediate ints and
		// range-checked them itself, which allocated per row and duplicated
		// what the standard library already guarantees.
		//
		// id is column 0, so database/sql's left-to-right Scan populates p.ID
		// whenever id itself is non-NULL before any later column's conversion
		// can fail; if id is NULL (SQLite allows NULL in a TEXT PRIMARY KEY
		// column declared without NOT NULL), Scan fails on column 0 with p.ID
		// left empty.
		var p dispatch.Persisted
		if err := rows.Scan(
			&p.ID, &p.SortKey,
			&p.Header.Name, &p.Header.Category, &p.Header.Priority, &p.Header.Bytes,
			&p.Policy.Verify, &p.Policy.Repair, &p.Policy.Unpack, &p.Policy.Delete,
			&p.State.State, &p.State.Next, &p.State.Activity, &p.State.Outcome,
			&p.State.Assessed, &p.Intent,
			&p.Header.Filename, &p.Header.IngestAnomaly, &p.Header.FailReason,
			&p.Header.DuplicateReason, &p.Header.OperationalError,
			&p.Header.Script, &p.Header.Password,
			&p.Header.PP, &p.Header.NZBBackup, &p.Header.URL, &p.Header.MD5,
			&p.Header.Added, &p.DownloadStarted, &p.DownloadFinished, &p.Par2ReleaseReason,
			&p.RecoveryBytes, &p.Par2Recovered, &p.Header.Unwanted,
		); err != nil {
			s.log.Error("dispatch/store: load scan failed", "job_id", p.ID, "err", err)
			continue
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dispatch/store: load: %w", err)
	}
	return out, nil
}

// Save writes one job's row, inserting or replacing it.
//
// It is an unconditional upsert over every column rather than a
// read-then-decide: persistIfChanged has already established that the row
// differs from what was last written, so a second comparison here would be
// work with no consumer — and it would put a second opinion about "has this
// changed?" behind the one that already owns the question.
func (s *Store) Save(ctx context.Context, p dispatch.Persisted) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dispatch_jobs (`+columns+`)
		 VALUES (?,?,?,?,?,?, ?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?,?,?,?,?,?,?, ?,?,?,?, ?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   sort_key=excluded.sort_key, name=excluded.name,
		   category=excluded.category, priority=excluded.priority,
		   bytes=excluded.bytes,
		   verify=excluded.verify, repair=excluded.repair,
		   unpack=excluded.unpack, delete_ok=excluded.delete_ok,
		   state=excluded.state, next=excluded.next,
		   activity=excluded.activity, outcome=excluded.outcome,
		   assessed=excluded.assessed, intent=excluded.intent,
		   filename=excluded.filename, ingest_anomaly=excluded.ingest_anomaly,
		   fail_reason=excluded.fail_reason,
		   duplicate_reason=excluded.duplicate_reason, operational_error=excluded.operational_error,
		   script=excluded.script, password=excluded.password,
		   pp=excluded.pp, nzb_backup=excluded.nzb_backup,
		   url=excluded.url, md5=excluded.md5,
		   added=excluded.added, download_started=excluded.download_started,
		   download_finished=excluded.download_finished,
		   par2_release_reason=excluded.par2_release_reason,
		   recovery_bytes=excluded.recovery_bytes,
		   par2_recovered=excluded.par2_recovered,
		   unwanted_ext=excluded.unwanted_ext`,
		p.ID, p.SortKey, p.Header.Name, p.Header.Category, p.Header.Priority, p.Header.Bytes,
		p.Policy.Verify, p.Policy.Repair, p.Policy.Unpack, p.Policy.Delete,
		p.State.State, p.State.Next, p.State.Activity, p.State.Outcome,
		p.State.Assessed, p.Intent,
		p.Header.Filename, p.Header.IngestAnomaly, p.Header.FailReason,
		p.Header.DuplicateReason, p.Header.OperationalError,
		p.Header.Script, p.Header.Password,
		p.Header.PP, p.Header.NZBBackup, p.Header.URL, p.Header.MD5,
		p.Header.Added, p.DownloadStarted, p.DownloadFinished, p.Par2ReleaseReason,
		p.RecoveryBytes, p.Par2Recovered, p.Header.Unwanted,
	)
	if err != nil {
		return fmt.Errorf("dispatch/store: save %s: %w", p.ID, err)
	}
	return nil
}

// Delete removes a job's row.
//
// Deleting a row that is not there is not an error, and that is a contract
// rather than an accident: evictCancelledNeverRun (dispatch/tick.go) treats a
// failed delete as the job's pass being over either way, so reporting absence
// would turn a redundant evict into a logged failure on every later tick.
func (s *Store) Delete(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM dispatch_jobs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("dispatch/store: delete %s: %w", id, err)
	}
	return nil
}

// Has reports whether a row with id exists in dispatch_jobs, including a row
// that Load skipped on a scan error.
func (s *Store) Has(ctx context.Context, id string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM dispatch_jobs WHERE id = ?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("dispatch/store: has %s: %w", id, err)
	}
	return true, nil
}
