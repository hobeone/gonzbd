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

	Name         string
	NzbName      string
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

// Add inserts e into the history table. It returns an error (wrapping a
// SQLite unique-constraint violation) if an entry with the same nzo_id already
// exists.
func (r *Repository) Add(ctx context.Context, e Entry) error {
	const q = `
INSERT INTO history
  (completed, name, nzb_name, category, pp, script, report, url, status,
   nzo_id, storage, path, script_log, script_line, download_time,
   postproc_time, stage_log, downloaded, completeness, fail_message,
   url_info, bytes, meta, series, md5sum, password, duplicate_key,
   archive, time_added)
VALUES
  (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

	_, err := r.db.ExecContext(ctx, q,
		toUnix(e.Completed),
		e.Name, e.NzbName, e.Category, e.PP, e.Script, e.Report,
		e.URL, e.Status, e.NzoID, e.Storage, e.Path,
		e.ScriptLog, e.ScriptLine,
		e.DownloadTime, e.PostprocTime, e.StageLog,
		e.Downloaded, e.Completeness, e.FailMessage, e.URLInfo,
		e.Bytes, e.Meta, e.Series, e.MD5Sum, e.Password,
		e.DuplicateKey, e.Archive, toUnix(e.TimeAdded),
	)
	if err != nil {
		return fmt.Errorf("history: add %q: %w", e.NzoID, err)
	}
	return nil
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
	var (
		where []string
		args  []any
	)

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
		where = append(where, "(name LIKE ? OR nzb_name LIKE ?)")
		like := "%" + opts.Search + "%"
		args = append(args, like, like)
	}
	if opts.MD5Sum != "" {
		where = append(where, "md5sum = ?")
		args = append(args, opts.MD5Sum)
	}

	q := "SELECT " + allColumns + " FROM history"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY completed DESC"
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", opts.Limit) //nolint:gosec // integer, not user string
	}
	if opts.Start > 0 {
		q += fmt.Sprintf(" OFFSET %d", opts.Start) //nolint:gosec // integer, not user string
	}

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("history: search: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only result set

	var out []Entry
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

// Count returns the total number of entries matching opts, ignoring Start and Limit.
func (r *Repository) Count(ctx context.Context, opts SearchOptions) (int, error) {
	var (
		where []string
		args  []any
	)

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
		where = append(where, "(name LIKE ? OR nzb_name LIKE ?)")
		like := "%" + opts.Search + "%"
		args = append(args, like, like)
	}
	if opts.MD5Sum != "" {
		where = append(where, "md5sum = ?")
		args = append(args, opts.MD5Sum)
	}

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

// Delete removes the entries identified by nzoIDs. It returns the number of
// rows actually deleted (IDs not present in the database are silently ignored).
// When multiple IDs are supplied the deletion is atomic.
func (r *Repository) Delete(ctx context.Context, nzoIDs ...string) (int, error) {
	if len(nzoIDs) == 0 {
		return 0, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("history: delete begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // superseded by Commit error

	placeholders := strings.Repeat("?,", len(nzoIDs))
	placeholders = placeholders[:len(placeholders)-1] // trim trailing comma

	args := make([]any, len(nzoIDs))
	for i, id := range nzoIDs {
		args[i] = id
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

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("history: delete commit: %w", err)
	}
	return int(n), nil
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

// Prune deletes history entries that have aged past the retention thresholds.
// retainDays applies to all non-failed entries; retainFailedDays applies only
// to entries whose status is 'Failed'. A value of 0 for either parameter means
// "keep forever" (spec §11.4). The deleted count covers both categories.
func (r *Repository) Prune(ctx context.Context, retainDays, retainFailedDays int) (int, error) {
	if retainDays == 0 && retainFailedDays == 0 {
		return 0, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("history: prune begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // superseded by Commit error

	total := 0

	if retainDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -retainDays).Unix()
		res, err := tx.ExecContext(ctx,
			"DELETE FROM history WHERE status != 'Failed' AND completed < ?", cutoff)
		if err != nil {
			return 0, fmt.Errorf("history: prune non-failed: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("history: prune non-failed rows affected: %w", err)
		}
		total += int(n)
	}

	if retainFailedDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -retainFailedDays).Unix()
		res, err := tx.ExecContext(ctx,
			"DELETE FROM history WHERE status = 'Failed' AND completed < ?", cutoff)
		if err != nil {
			return 0, fmt.Errorf("history: prune failed: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("history: prune failed rows affected: %w", err)
		}
		total += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("history: prune commit: %w", err)
	}
	return total, nil
}

// allColumns is the canonical SELECT column list, ordered to match scanEntry.
const allColumns = `id, completed, name, nzb_name, category, pp, script, report,
url, status, nzo_id, storage, path, script_log, script_line, download_time,
postproc_time, stage_log, downloaded, completeness, fail_message, url_info,
bytes, meta, series, md5sum, password, duplicate_key, archive, time_added`

// scanner abstracts over *sql.Row and *sql.Rows so scanEntry works for both.
type scanner interface {
	Scan(dest ...any) error
}

// scanEntry reads one history row into an Entry. Timestamp columns are stored
// as unix seconds (INTEGER) and converted to time.Time using UTC.
func scanEntry(s scanner) (*Entry, error) {
	var (
		e                    Entry
		completed, timeAdded int64
	)
	err := s.Scan(
		&e.ID, &completed,
		&e.Name, &e.NzbName, &e.Category, &e.PP, &e.Script, &e.Report,
		&e.URL, &e.Status, &e.NzoID, &e.Storage, &e.Path,
		&e.ScriptLog, &e.ScriptLine,
		&e.DownloadTime, &e.PostprocTime, &e.StageLog,
		&e.Downloaded, &e.Completeness, &e.FailMessage, &e.URLInfo,
		&e.Bytes, &e.Meta, &e.Series, &e.MD5Sum, &e.Password,
		&e.DuplicateKey, &e.Archive, &timeAdded,
	)
	if err != nil {
		return nil, err
	}
	e.Completed = fromUnix(completed)
	e.TimeAdded = fromUnix(timeAdded)
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
