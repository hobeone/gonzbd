package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned by Get when no history entry matches the requested
// nzo_id.
var ErrNotFound = errors.New("history: entry not found")

// Entry mirrors one row in the history table. INTEGER columns that carry unix
// timestamps are exposed as time.Time for ergonomic use by callers; the
// repository converts to/from unix seconds on every read and write. Columns
// that are frequently unset use plain string/int64 types rather than
// sql.Null* to keep the API simple — SQL NULL round-trips as zero value.
type Entry struct {
	// ID is the auto-assigned SQLite row id; zero on insertion.
	ID int64

	// Completed holds the unix timestamp of when the job finished.
	Completed time.Time

	Name    string
	NzbName string

	// NZBBackup is the basename of the gzipped NZB backup under admin/nzb/
	// that this job was written to at add time. Retry re-parses it to
	// recover the article message-IDs, which survive nowhere else once the
	// job's manifest is unlinked at finalization.
	//
	// Deliberately separate from NzbName: that field holds the submitted
	// filename and is a compatibility surface (the mode=history response,
	// the UI history row, the post-processing script's second positional
	// argument, and the search predicate), while the backup's own name
	// takes a .1/.2 suffix when a forced duplicate add would otherwise
	// overwrite an existing backup. Empty for entries written before the
	// backup became load-bearing.
	NZBBackup string

	Category     string
	PP           string
	Script       string
	Report       string
	URL          string
	Status       string
	NzoID        string
	Storage      string
	Path         string
	ScriptLog    []byte
	ScriptLine   string
	DownloadTime int64
	PostprocTime int64

	// StageLog stores a JSON-encoded list of post-processing stage records,
	// as produced by the Python implementation. The repository treats it as
	// an opaque string; callers are responsible for encoding/decoding JSON.
	StageLog     string
	Downloaded   int64
	Completeness int64
	FailMessage  string
	URLInfo      string
	Bytes        int64
	Meta         string
	Series       string
	MD5Sum       string
	Password     string
	DuplicateKey string
	Archive      int64

	// TimeAdded holds the unix timestamp of when the job was added to the queue.
	TimeAdded time.Time
}

// SearchOptions controls which rows Search returns.
type SearchOptions struct {
	// Status filters by exact status string. Empty means no filter.
	Status string
	// Category filters by exact category string. Empty means no filter.
	Category string
	// Search is applied as a case-insensitive LIKE substring match against
	// the name and nzb_name columns. Empty means no filter.
	Search string
	// Start is the zero-based offset for pagination.
	Start int
	// Limit is the maximum number of rows to return. 0 means no limit.
	Limit int
	// ArchiveOnly restricts results to rows where archive != 0.
	ArchiveOnly bool
	// MD5Sum filters by exact MD5 hash string. Empty means no filter.
	MD5Sum string
}

// Repository provides CRUD access to the history table. A zero-value
// Repository is not usable; construct one via NewRepository.
type Repository struct {
	db *sql.DB
}

// NewRepository wraps an open DB for use as a repository.
func NewRepository(d *DB) *Repository {
	return &Repository{db: d.db}
}

// Execer represents a SQL executor (either *sql.DB or *sql.Tx).
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// DB returns the underlying sql.DB connection pool.
func (r *Repository) DB() *sql.DB {
	return r.db
}

// AddTx inserts e into the history table using an open SQL executor (either a transaction or DB pool).
func (r *Repository) AddTx(ctx context.Context, exec Execer, e Entry) error {
	const q = `
INSERT INTO history
  (completed, name, nzb_name, category, pp, script, report, url, status,
   nzo_id, storage, path, script_log, script_line, download_time,
   postproc_time, stage_log, downloaded, completeness, fail_message,
   url_info, bytes, meta, series, md5sum, password, duplicate_key,
   archive, time_added, nzb_backup)
VALUES
  (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

	_, err := exec.ExecContext(ctx, q,
		toUnix(e.Completed),
		e.Name, e.NzbName, e.Category, e.PP, e.Script, e.Report,
		e.URL, e.Status, e.NzoID, e.Storage, e.Path,
		e.ScriptLog, e.ScriptLine,
		e.DownloadTime, e.PostprocTime, e.StageLog,
		e.Downloaded, e.Completeness, e.FailMessage, e.URLInfo,
		e.Bytes, e.Meta, e.Series, e.MD5Sum, e.Password,
		e.DuplicateKey, e.Archive, toUnix(e.TimeAdded), e.NZBBackup,
	)
	if err != nil {
		return fmt.Errorf("history: add tx %q: %w", e.NzoID, err)
	}
	return nil
}

// Add inserts e into the history table. It returns an error (wrapping a
// SQLite unique-constraint violation) if an entry with the same nzo_id already
// exists.
func (r *Repository) Add(ctx context.Context, e Entry) error {
	return r.AddTx(ctx, r.db, e)
}

// Get fetches the entry with the given nzo_id. It returns ErrNotFound (via
// errors.Is) when no matching row exists.
func (r *Repository) Get(ctx context.Context, nzoID string) (*Entry, error) {
	const q = `SELECT ` + allColumns + ` FROM history WHERE nzo_id = ?`
	row := r.db.QueryRowContext(ctx, q, nzoID)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("history: get %q: %w", nzoID, err)
	}
	return e, nil
}

// Search returns entries matching opts. Filters are ANDed together. Results
// are ordered by completed DESC (most-recent first), matching the upstream
// API's default sort for the history endpoint (spec §10).
func (r *Repository) Search(ctx context.Context, opts SearchOptions) ([]Entry, error) {
	where, args := buildWhereClause(opts)

	q := "SELECT " + allColumns + " FROM history"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY completed DESC"
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", opts.Limit) //nolint:gosec // integer, not user string
	} else if opts.Start > 0 {
		q += " LIMIT -1" // SQLite requires LIMIT when OFFSET is used; -1 means unlimited
	}
	if opts.Start > 0 {
		q += fmt.Sprintf(" OFFSET %d", opts.Start) //nolint:gosec // integer, not user string
	}

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("history: search: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only result set

	// Preallocate to the requested page size when known (OPT-11); the
	// query can never return more than opts.Limit rows in that case. Fall
	// back to a small hint for unbounded queries so we still avoid the
	// first few reallocations. Cap the preallocation independently of the
	// SQL LIMIT: opts.Limit is not bounded at this boundary, so an
	// attacker- or caller-supplied huge value would otherwise allocate
	// that many Entry structs up front, before a single row is read.
	const maxPrealloc = 10_000
	capHint := 16
	if opts.Limit > 0 {
		capHint = min(opts.Limit, maxPrealloc)
	}
	out := make([]Entry, 0, capHint)
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("history: search scan: %w", err)
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: search rows: %w", err)
	}
	return out, nil
}

// ListIDs returns just the nzo_id values matching opts, avoiding the cost of
// loading full Entry structs. Used by bulk delete operations.
func (r *Repository) ListIDs(ctx context.Context, opts SearchOptions) ([]string, error) {
	where, args := buildWhereClause(opts)

	q := "SELECT nzo_id FROM history"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // G202: clauses are internally built, not user input
	}

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("history: list ids: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only result set

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("history: list ids scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: list ids rows: %w", err)
	}
	return ids, nil
}

// buildWhereClause constructs the WHERE predicates and args for Search/Count
// queries from SearchOptions. Centralizes filter logic so new filters only
// need to be added in one place.
func buildWhereClause(opts SearchOptions) (where []string, args []any) {
	if opts.ArchiveOnly {
		where = append(where, "archive != 0")
	}
	if opts.Status != "" {
		where = append(where, "status = ?")
		args = append(args, opts.Status)
	}
	if opts.Category != "" {
		where = append(where, "category = ?")
		args = append(args, opts.Category)
	}
	if opts.Search != "" {
		where = append(where, "(name LIKE ? ESCAPE '\\' OR nzb_name LIKE ? ESCAPE '\\')")
		like := "%" + escapeLike(opts.Search) + "%"
		args = append(args, like, like)
	}
	if opts.MD5Sum != "" {
		where = append(where, "md5sum = ?")
		args = append(args, opts.MD5Sum)
	}
	return where, args
}

// likeReplacer escapes SQL LIKE special characters (%, _, \) so they
// are matched literally. Built once at package init.
var likeReplacer = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// escapeLike escapes SQL LIKE special characters in s so they
// are matched literally. The caller must add ESCAPE '\\' to the LIKE clause.
func escapeLike(s string) string {
	return likeReplacer.Replace(s)
}

// Count returns the total number of entries matching opts, ignoring Start and Limit.
func (r *Repository) Count(ctx context.Context, opts SearchOptions) (int, error) {
	where, args := buildWhereClause(opts)

	q := "SELECT COUNT(*) FROM history"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}

	var count int
	if err := r.db.QueryRowContext(ctx, q, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("history: count: %w", err)
	}
	return count, nil
}

// Delete removes the entries identified by nzoIDs, and with them everything
// the entry owned: its retained per-file progress and its durability rows. It
// returns the number of rows actually deleted (IDs not present in the database
// are silently ignored). When multiple IDs are supplied the deletion is atomic.
// Large batches are chunked to stay under SQLite's SQLITE_MAX_VARIABLE_NUMBER
// limit.
//
// Use DeleteKeepingDurability when the job is going back into the queue.
func (r *Repository) Delete(ctx context.Context, nzoIDs ...string) (int, error) {
	return r.delete(ctx, true, nzoIDs...)
}

// DeleteKeepingDurability removes the entries but leaves article_facts and
// file_extents in place.
//
// It exists for exactly one caller: a retry, which re-enqueues the job under
// the SAME ID. Those rows are what bound the retry's truncate to the whole
// partial file rather than to the handful of articles it re-fetches, and
// deleting them is #422 — the retention job_finalizer.go maintains, destroyed
// by the one path that was supposed to consume it.
//
// It is a separate method rather than a bool on Delete because every other
// caller is deleting an entry that is GONE, and for those the rows must go
// too or they accumulate one set per download ever performed. A bool would
// put that decision at each call site; a name puts it in the type.
func (r *Repository) DeleteKeepingDurability(ctx context.Context, nzoIDs ...string) (int, error) {
	return r.delete(ctx, false, nzoIDs...)
}

func (r *Repository) delete(ctx context.Context, dropDurability bool, nzoIDs ...string) (int, error) {
	if len(nzoIDs) == 0 {
		return 0, nil
	}

	const chunkSize = 999 // SQLite safe limit for host parameters

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("history: delete begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // superseded by Commit error

	totalDeleted := 0
	for i := 0; i < len(nzoIDs); i += chunkSize {
		end := min(i+chunkSize, len(nzoIDs))
		chunk := nzoIDs[i:end]

		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1] // trim trailing comma

		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}

		// Retained per-file progress is owned by the history entry but has
		// no foreign key to cascade from, because the jobs row it used to
		// hang off is deleted at MoveToHistory. Removing it here rather
		// than at Delete's call sites is what keeps it from accumulating:
		// every deletion path, present and future, gets the cleanup without
		// having to remember it, in the same transaction as the row itself.
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM history_job_files WHERE job_id IN ("+placeholders+")", args...); err != nil { //nolint:gosec // placeholders is only "?,?,?" — no user data
			return 0, fmt.Errorf("history: delete retained job files: %w", err)
		}

		// A failed job's Class A facts and Class B extents are retained so a
		// retry can bound its truncate to the whole partial file rather than
		// to the articles it re-fetched. Ownership passes to the history entry
		// while the job sits there, and they share its lack of a foreign key
		// to cascade from, so an entry that is going away for good takes them
		// with it — every such deletion path gets the cleanup without having
		// to remember it.
		//
		// Ownership passes BACK to the job when it re-enters the queue, which
		// is why this is conditional and why DeleteKeepingDurability exists.
		// An earlier version deleted unconditionally and justified it with
		// "a completed job's rows are already gone, so this deletes nothing
		// for it" — true of the completed case and silent about the failed
		// one, which is the only case the retention was for. The retry called
		// straight through here and destroyed the rows it needed (#422).
		//
		// Still unconditional on STATUS within this branch: a completed job's
		// rows are already gone, so making status explicit here would mean
		// this cleanup and the one that wrote them had to agree about it
		// forever.
		if dropDurability {
			for _, table := range []string{"article_facts", "file_extents"} {
				if _, err := tx.ExecContext(ctx,
					"DELETE FROM "+table+" WHERE job_id IN ("+placeholders+")", args...); err != nil { //nolint:gosec // table is a literal from the slice above; placeholders is only "?,?,?"
					return 0, fmt.Errorf("history: delete retained durability rows from %s: %w", table, err)
				}
			}
		}

		res, err := tx.ExecContext(ctx,
			"DELETE FROM history WHERE nzo_id IN ("+placeholders+")", args...) //nolint:gosec // placeholders is only "?,?,?" — no user data
		if err != nil {
			return 0, fmt.Errorf("history: delete: %w", err)
		}

		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("history: delete rows affected: %w", err)
		}
		totalDeleted += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("history: delete commit: %w", err)
	}
	return totalDeleted, nil
}

// MarkCompleted sets status = 'Completed' and completed = now for the entry
// identified by nzoID. It is used by the "mark_as_completed" API endpoint.
func (r *Repository) MarkCompleted(ctx context.Context, nzoID string) error {
	res, err := r.db.ExecContext(ctx,
		"UPDATE history SET status = 'Completed', completed = ? WHERE nzo_id = ?",
		time.Now().Unix(), nzoID,
	)
	if err != nil {
		return fmt.Errorf("history: mark completed %q: %w", nzoID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("history: mark completed rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ExpiredEntries returns the entries that have aged past the retention
// thresholds, oldest first. retainDays applies to every non-failed entry;
// retainFailedDays applies only to entries whose status is 'Failed'. A value
// of 0 for either means "keep forever" (spec §11.4), and 0 for both returns
// nothing without touching the database.
//
// This deliberately selects rather than deletes. An entry owns two things
// besides its row — its history_job_files progress and its admin/nzb backup —
// and only the caller can release the second, because it is a file and this
// package has no business in the admin directory. Handing the entries back
// lets one deletion path release all three, instead of a pruner that quietly
// orphans two of them (#303).
func (r *Repository) ExpiredEntries(ctx context.Context, retainDays, retainFailedDays int) ([]Entry, error) {
	if retainDays <= 0 && retainFailedDays <= 0 {
		return nil, nil
	}

	// Static SQL with the thresholds bound as parameters, rather than a
	// WHERE clause assembled from string fragments: a disabled threshold
	// switches its half off through the flag instead of by omitting text.
	nonFailedOn, failedOn := 0, 0
	var nonFailedCutoff, failedCutoff int64
	if retainDays > 0 {
		nonFailedOn = 1
		nonFailedCutoff = time.Now().AddDate(0, 0, -retainDays).Unix()
	}
	if retainFailedDays > 0 {
		failedOn = 1
		failedCutoff = time.Now().AddDate(0, 0, -retainFailedDays).Unix()
	}

	const q = `SELECT ` + allColumns + `
FROM history
WHERE (? = 1 AND status != 'Failed' AND completed < ?)
   OR (? = 1 AND status =  'Failed' AND completed < ?)
ORDER BY completed ASC`

	rows, err := r.db.QueryContext(ctx, q, nonFailedOn, nonFailedCutoff, failedOn, failedCutoff)
	if err != nil {
		return nil, fmt.Errorf("history: expired entries: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only result set

	var out []Entry
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("history: expired entries scan: %w", scanErr)
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: expired entries rows: %w", err)
	}
	return out, nil
}

// allColumns is the canonical SELECT column list, ordered to match scanEntry.
const allColumns = `id, completed, name, nzb_name, category, pp, script, report,
url, status, nzo_id, storage, path, script_log, script_line, download_time,
postproc_time, stage_log, downloaded, completeness, fail_message, url_info,
bytes, meta, series, md5sum, password, duplicate_key, archive, time_added,
nzb_backup`

// scanner abstracts over *sql.Row and *sql.Rows so scanEntry works for both.
type scanner interface {
	Scan(dest ...any) error
}

// scanEntry reads one history row into an Entry. Timestamp columns are stored
// as unix seconds (INTEGER) and converted to time.Time using UTC.
//
// TRACE-4: the schema (internal/history/migrations/001_initial.sql) declares
// every TEXT/INTEGER column nullable, but Entry exposes plain string/int64
// fields, not sql.Null*. Add() always binds concrete zero-valued (non-NULL)
// values, so app-written rows never contain NULLs in practice — but a row
// inserted by any other means (manual sqlite3, a future migration, an
// external tool) with a NULL column would otherwise make database/sql return
// "converting NULL to string/int64 is unsupported", breaking Get/Search for
// the whole result set. Scan through sql.Null* and coalesce NULL to the zero
// value, matching the "SQL NULL round-trips as zero value" contract already
// documented on the Entry struct.
func scanEntry(s scanner) (*Entry, error) {
	var (
		e                                                                    Entry
		completed, timeAdded                                                 sql.NullInt64
		name, nzbName, category, pp, script, report, urlField, status, nzoID sql.NullString
		storage, path, scriptLine, stageLog, failMessage, urlInfo            sql.NullString
		meta, series, md5sum, password, duplicateKey, nzbBackup              sql.NullString
		downloadTime, postprocTime, downloaded, completeness                 sql.NullInt64
		bytesVal, archive                                                    sql.NullInt64
	)
	err := s.Scan(
		&e.ID, &completed,
		&name, &nzbName, &category, &pp, &script, &report,
		&urlField, &status, &nzoID, &storage, &path,
		&e.ScriptLog, &scriptLine,
		&downloadTime, &postprocTime, &stageLog,
		&downloaded, &completeness, &failMessage, &urlInfo,
		&bytesVal, &meta, &series, &md5sum, &password,
		&duplicateKey, &archive, &timeAdded, &nzbBackup,
	)
	if err != nil {
		return nil, err
	}
	e.Completed = fromUnix(completed.Int64)
	e.TimeAdded = fromUnix(timeAdded.Int64)
	e.Name = name.String
	e.NzbName = nzbName.String
	e.Category = category.String
	e.PP = pp.String
	e.Script = script.String
	e.Report = report.String
	e.URL = urlField.String
	e.Status = status.String
	e.NzoID = nzoID.String
	e.Storage = storage.String
	e.Path = path.String
	e.ScriptLine = scriptLine.String
	e.DownloadTime = downloadTime.Int64
	e.PostprocTime = postprocTime.Int64
	e.StageLog = stageLog.String
	e.Downloaded = downloaded.Int64
	e.Completeness = completeness.Int64
	e.FailMessage = failMessage.String
	e.URLInfo = urlInfo.String
	e.Bytes = bytesVal.Int64
	e.Meta = meta.String
	e.Series = series.String
	e.MD5Sum = md5sum.String
	e.Password = password.String
	e.DuplicateKey = duplicateKey.String
	e.Archive = archive.Int64
	e.NZBBackup = nzbBackup.String
	return &e, nil
}

// toUnix converts t to a unix timestamp. A zero time becomes 0, which SQLite
// stores as NULL-equivalent for the Python compatibility layer.
func toUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// fromUnix converts a unix timestamp to time.Time in UTC. 0 maps to zero time.
func fromUnix(ts int64) time.Time {
	if ts == 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0).UTC()
}

// Ping verifies that the underlying database connection is alive.
func (r *Repository) Ping(ctx context.Context) error {
	if r == nil || r.db == nil {
		return errors.New("history: repository or db connection is nil")
	}
	if err := r.db.PingContext(ctx); err != nil {
		return fmt.Errorf("history: ping: %w", err)
	}
	return nil
}
