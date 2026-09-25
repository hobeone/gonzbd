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
	MD5Sum       string
	Password     string
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
  (completed, name, nzb_name, category, pp, script, url, status,
   nzo_id, storage, path, script_log, script_line, download_time,
   postproc_time, stage_log, downloaded, completeness, fail_message,
   url_info, bytes, meta, md5sum, password,
   archive, time_added, nzb_backup)
VALUES
  (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

	_, err := exec.ExecContext(ctx, q,
		toUnix(e.Completed),
		e.Name, e.NzbName, e.Category, e.PP, e.Script,
		e.URL, e.Status, e.NzoID, e.Storage, e.Path,
		e.ScriptLog, e.ScriptLine,
		e.DownloadTime, e.PostprocTime, e.StageLog,
		e.Downloaded, e.Completeness, e.FailMessage, e.URLInfo,
		e.Bytes, e.Meta, e.MD5Sum, e.Password,
		e.Archive, toUnix(e.TimeAdded), e.NZBBackup,
	)
	if err != nil {
		return fmt.Errorf("history: add tx %q: %w", e.NzoID, err)
	}
	return nil
}

// FileProgress is one file's retained download progress: what a retry needs to
// resume the file instead of re-fetching it.
//
// Every field is written by Add and read back by RetainedFiles, so none of them
// is populated only on one of those paths.
type FileProgress struct {
	FileIndex      int
	Complete       bool
	FetchPolicy    uint8
	Filename       string
	AssembledCRC32 uint32
	ArticleCount   int
}

// Add inserts e and its retained per-file progress in one transaction. It
// returns an error (wrapping a SQLite unique-constraint violation) if an entry
// with the same nzo_id already exists.
//
// Both land or neither does. An entry whose progress did not land sends a retry
// back to re-fetch every article the failed attempt had already written, since
// these rows are the only surviving record of it once the job's manifest is
// gone — and a retry reads them through RetainedFiles.
//
// The rows take their job_id from e.NzoID rather than from a second argument,
// so an entry and its progress cannot be filed under different ids.
func (r *Repository) Add(ctx context.Context, e Entry, files []FileProgress) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("history: add %q: begin: %w", e.NzoID, err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // superseded by Commit error

	if err := r.AddTx(ctx, tx, e); err != nil {
		return err
	}
	if err := addFileProgressTx(ctx, tx, e.NzoID, files); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("history: add %q: commit: %w", e.NzoID, err)
	}
	return nil
}

// addFileProgressTx writes one row per file.
//
// A plain INSERT, not INSERT OR REPLACE. Rows for this job_id exist only
// alongside an entry with the same nzo_id, which delete removes in one
// transaction with them — and if such an entry were present, AddTx above would
// already have failed on the UNIQUE nzo_id and this would not be reached. What
// is left for the PRIMARY KEY (job_id, file_index) to catch is a caller passing
// one file index twice, which a replace would silently absorb.
func addFileProgressTx(ctx context.Context, exec Execer, jobID string, files []FileProgress) error {
	const q = `
INSERT INTO history_job_files
  (job_id, file_index, complete, fetch_policy, filename, assembled_crc32, article_count)
VALUES (?, ?, ?, ?, ?, ?, ?)`
	for _, f := range files {
		complete := 0
		if f.Complete {
			complete = 1
		}
		if _, err := exec.ExecContext(ctx, q,
			jobID, f.FileIndex, complete, int(f.FetchPolicy),
			f.Filename, f.AssembledCRC32, f.ArticleCount,
		); err != nil {
			return fmt.Errorf("history: add retained progress %q file %d: %w", jobID, f.FileIndex, err)
		}
	}
	return nil
}

// RetainedFiles returns the per-file progress Add stored with jobID's entry,
// ordered by file index. An entry with none yields no rows and no error.
func (r *Repository) RetainedFiles(ctx context.Context, jobID string) ([]FileProgress, error) {
	const q = `
SELECT file_index, complete, fetch_policy,
       COALESCE(filename, ''), COALESCE(assembled_crc32, 0), article_count
FROM history_job_files WHERE job_id = ? ORDER BY file_index ASC`
	rows, err := r.db.QueryContext(ctx, q, jobID)
	if err != nil {
		return nil, fmt.Errorf("history: retained files %q: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []FileProgress
	for rows.Next() {
		var f FileProgress
		var complete int
		if err := rows.Scan(&f.FileIndex, &complete, &f.FetchPolicy,
			&f.Filename, &f.AssembledCRC32, &f.ArticleCount); err != nil {
			return nil, fmt.Errorf("history: scan retained file %q: %w", jobID, err)
		}
		f.Complete = complete != 0
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: retained files %q: %w", jobID, err)
	}
	return out, nil
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

// Delete removes the entries identified by nzoIDs, and with them their
// retained per-file progress, in one transaction. It returns the number of
// rows actually deleted (IDs not present in the database are silently
// ignored). Large batches are chunked to stay under SQLite's
// SQLITE_MAX_VARIABLE_NUMBER limit.
//
// A job's durability rows are not this package's. Of the two callers
// (`git grep -n 'historyRepo.Delete(' -- 'internal/app/*.go' ':!*_test.go'`
// returns 2 lines), deleteHistoryEntries reclaims them through durability.Store.Reclaim
// once the entry is gone, and RetryHistoryJob deliberately does not: it has
// just put the job back in the queue, which is what the rule reads.
func (r *Repository) Delete(ctx context.Context, nzoIDs ...string) (int, error) {
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
const allColumns = `id, completed, name, nzb_name, category, pp, script,
url, status, nzo_id, storage, path, script_log, script_line, download_time,
postproc_time, stage_log, downloaded, completeness, fail_message, url_info,
bytes, meta, md5sum, password, archive, time_added,
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
		e                                                            Entry
		completed, timeAdded                                         sql.NullInt64
		name, nzbName, category, pp, script, urlField, status, nzoID sql.NullString
		storage, path, scriptLine, stageLog, failMessage, urlInfo    sql.NullString
		meta, md5sum, password, nzbBackup                            sql.NullString
		downloadTime, postprocTime, downloaded, completeness         sql.NullInt64
		bytesVal, archive                                            sql.NullInt64
	)
	err := s.Scan(
		&e.ID, &completed,
		&name, &nzbName, &category, &pp, &script,
		&urlField, &status, &nzoID, &storage, &path,
		&e.ScriptLog, &scriptLine,
		&downloadTime, &postprocTime, &stageLog,
		&downloaded, &completeness, &failMessage, &urlInfo,
		&bytesVal, &meta, &md5sum, &password,
		&archive, &timeAdded, &nzbBackup,
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
	e.MD5Sum = md5sum.String
	e.Password = password.String
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
