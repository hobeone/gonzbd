package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// SQLiteStore implements Store using an active SQLite database connection pool.
type SQLiteStore struct {
	db          *sql.DB
	dir         string
	historyRepo *history.Repository
}

// NewSQLiteStore constructs a SQLiteStore backed by db and storing manifests inside dir.
func NewSQLiteStore(db *sql.DB, dir string, historyRepo *history.Repository) *SQLiteStore {
	return &SQLiteStore{
		db:          db,
		dir:         dir,
		historyRepo: historyRepo,
	}
}

// Add inserts a new active job into the store and writes its manifest to disk.
func (s *SQLiteStore) Add(ctx context.Context, job *Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite store begin tx add: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sortKey int
	row := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sort_key), -1) + 1 FROM jobs")
	if err := row.Scan(&sortKey); err != nil {
		return fmt.Errorf("sqlite store max sort_key: %w", err)
	}

	groupsJSON, _ := json.Marshal(job.Groups)
	metaJSON, _ := json.Marshal(job.Meta)

	const qJobs = `
INSERT INTO jobs
  (id, filename, name, password, url, category, priority, status, pp, script,
   time_added, md5, avg_age, groups, meta, warning, postproc, sort_key)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	postprocInt := 0
	if job.PostProc {
		postprocInt = 1
	}

	_, err = tx.ExecContext(ctx, qJobs,
		job.ID, job.Filename, job.Name, job.Password, job.URL, job.Category,
		int(job.Priority), string(job.Status), job.PP, job.Script,
		job.Added.Unix(), job.MD5, job.AvgAge.Unix(), string(groupsJSON), string(metaJSON),
		job.Warning, postprocInt, sortKey,
	)
	if err != nil {
		return fmt.Errorf("sqlite store insert job %s: %w", job.ID, err)
	}

	if job.Manifest() != nil {
		const qFiles = `
INSERT INTO job_files
  (job_id, file_index, subject, date, bytes, is_par2_recovery, complete, deferred, write_cursor, bytes_downloaded, filename, assembled_crc32)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		for i := range job.Manifest().NumFiles() {
			isPar2 := 0
			if job.Manifest().FileIsPar2Recovery(i) {
				isPar2 = 1
			}
			_, err = tx.ExecContext(ctx, qFiles,
				job.ID, i, job.Manifest().FileSubject(i), job.Manifest().FileDate(i).Unix(), job.Manifest().FileBytes(i), isPar2, 0, 0, 0, 0, "", 0,
			)
			if err != nil {
				return fmt.Errorf("sqlite store insert job_file %s/%d: %w", job.ID, i, err)
			}
		}

		manifestPath := filepath.Join(s.dir, "manifests", job.ID+".json.gz")
		if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
			return fmt.Errorf("sqlite store mkdir manifests: %w", err)
		}
		if err := writeGzJSON(manifestPath, job.Manifest()); err != nil {
			return fmt.Errorf("sqlite store write manifest %s: %w", job.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite store commit add %s: %w", job.ID, err)
	}
	return nil
}

// Get retrieves an active job by ID, reconstructing it from SQLite and its manifest.
func (s *SQLiteStore) Get(ctx context.Context, id string) (*Job, error) {
	const qJob = `
SELECT id, filename, name, password, url, category, priority, status, pp, script,
       time_added, md5, avg_age, groups, meta, warning, postproc
FROM jobs WHERE id = ?`

	var job Job
	var groupsStr, metaStr, statusStr string
	var priorityInt, ppInt, postprocInt int
	var addedUnix, avgAgeUnix int64

	err := s.db.QueryRowContext(ctx, qJob, id).Scan(
		&job.ID, &job.Filename, &job.Name, &job.Password, &job.URL, &job.Category,
		&priorityInt, &statusStr, &ppInt, &job.Script, &addedUnix, &job.MD5, &avgAgeUnix,
		&groupsStr, &metaStr, &job.Warning, &postprocInt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite store get job %s: %w", id, err)
	}

	job.Priority = constants.Priority(priorityInt) //nolint:gosec // G115: priority fits in int8
	job.Status = constants.Status(statusStr)
	job.PP = ppInt
	job.Added = time.Unix(addedUnix, 0)
	job.AvgAge = time.Unix(avgAgeUnix, 0)
	job.PostProc = postprocInt != 0

	if groupsStr != "" {
		_ = json.Unmarshal([]byte(groupsStr), &job.Groups)
	}
	if metaStr != "" {
		_ = json.Unmarshal([]byte(metaStr), &job.Meta)
	}

	manifestPath := filepath.Join(s.dir, "manifests", id+".json.gz")
	var manifest Manifest
	if err := readGzJSON(manifestPath, &manifest); err == nil {
		job.manifest = &manifest
		job.progress = newJobProgress(&manifest)

		const qFiles = `
SELECT file_index, complete, deferred, write_cursor, bytes_downloaded, assembled_crc32
FROM job_files WHERE job_id = ? ORDER BY file_index ASC`
		rows, err := s.db.QueryContext(ctx, qFiles, id)
		if err == nil {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var idx, complete, deferred int
				var writeCursor, bytesDownloaded int64
				var crc32Val uint32
				if err := rows.Scan(&idx, &complete, &deferred, &writeCursor, &bytesDownloaded, &crc32Val); err == nil {
					if idx >= 0 && idx < len(job.progress.files) {
						fp := &job.progress.files[idx]
						fp.BytesDownloaded = bytesDownloaded
						fp.WriteCursor = writeCursor
						fp.AssembledCRC32 = crc32Val
						if complete != 0 {
							fp.Complete = true
							lo, hi := job.manifest.FileRange(idx)
							for a := lo; a < hi; a++ {
								job.progress.markDone(job.manifest, a)
							}
						}
						if deferred != 0 {
							fp.Deferred = true
						}
					}
				}
			}
			_ = rows.Err()
		}
	}

	return &job, nil
}

// List returns all active jobs ordered by sort_key ASC, time_added ASC.
func (s *SQLiteStore) List(ctx context.Context) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM jobs ORDER BY sort_key ASC, time_added ASC")
	if err != nil {
		return nil, fmt.Errorf("sqlite store list query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite store list rows: %w", err)
	}

	jobs := make([]*Job, 0, len(ids))
	for _, id := range ids {
		job, err := s.Get(ctx, id)
		if err == nil && job != nil {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}

// Update modifies an existing active job's live metadata in SQLite.
func (s *SQLiteStore) Update(ctx context.Context, job *Job) error {
	const q = `
UPDATE jobs SET
  name = ?, category = ?, priority = ?, status = ?, pp = ?, script = ?, warning = ?, postproc = ?
WHERE id = ?`

	postprocInt := 0
	if job.PostProc {
		postprocInt = 1
	}

	res, err := s.db.ExecContext(ctx, q,
		job.Name, job.Category, int(job.Priority), string(job.Status), job.PP, job.Script, job.Warning, postprocInt, job.ID,
	)
	if err != nil {
		return fmt.Errorf("sqlite store update job %s: %w", job.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}

	if job.Progress() != nil && job.Manifest() != nil {
		const qF = `UPDATE job_files SET complete = ?, deferred = ?, write_cursor = ?, bytes_downloaded = ?, filename = ?, assembled_crc32 = ? WHERE job_id = ? AND file_index = ?`
		for i := range job.Manifest().NumFiles() {
			complete := 0
			if job.Progress().FileComplete(i) {
				complete = 1
			}
			deferred := 0
			if job.Progress().FileDeferred(i) {
				deferred = 1
			}
			_, _ = s.db.ExecContext(ctx, qF,
				complete, deferred, job.Progress().FileWriteCursor(i), job.Progress().FileBytesDownloaded(i), job.Progress().FileFilename(i), job.Progress().FileAssembledCRC32(i),
				job.ID, i,
			)
		}
	}

	return nil
}

// Remove deletes an active job and its child files, removing any filesystem manifests.
func (s *SQLiteStore) Remove(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite store begin tx remove: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const qFiles = "DELETE FROM job_files WHERE job_id = ?"
	if _, err := tx.ExecContext(ctx, qFiles, id); err != nil {
		return fmt.Errorf("sqlite store delete job_files %s: %w", id, err)
	}

	const qJob = "DELETE FROM jobs WHERE id = ?"
	res, err := tx.ExecContext(ctx, qJob, id)
	if err != nil {
		return fmt.Errorf("sqlite store delete job %s: %w", id, err)
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite store commit remove %s: %w", id, err)
	}

	_ = os.Remove(filepath.Join(s.dir, "manifests", id+".json.gz"))
	_ = os.Remove(filepath.Join(s.dir, "progress", id+".json.gz"))
	return nil
}

// MoveToHistory atomically transitions an active job into history within a single SQLite transaction.
func (s *SQLiteStore) MoveToHistory(ctx context.Context, job *Job, entry history.Entry) error {
	if s.historyRepo == nil {
		return errors.New("sqlite store: history repository not wired")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite store begin tx movetohistory %s: %w", job.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.historyRepo.AddTx(ctx, tx, entry); err != nil {
		return fmt.Errorf("sqlite store add history %s: %w", job.ID, err)
	}

	_, _ = tx.ExecContext(ctx, "DELETE FROM job_files WHERE job_id = ?", job.ID)
	_, err = tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", job.ID)
	if err != nil {
		return fmt.Errorf("sqlite store delete active job %s: %w", job.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite store commit movetohistory %s: %w", job.ID, err)
	}

	_ = os.Remove(filepath.Join(s.dir, "manifests", job.ID+".json.gz"))
	_ = os.Remove(filepath.Join(s.dir, "progress", job.ID+".json.gz"))
	return nil
}

// ExistsByName checks if an active job with the given name exists using an index.
func (s *SQLiteStore) ExistsByName(ctx context.Context, name string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE name = ?)", name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("sqlite store existsbyname: %w", err)
	}
	return exists != 0, nil
}

// ExistsByMD5 checks if an active job with the given MD5 exists using an index.
func (s *SQLiteStore) ExistsByMD5(ctx context.Context, md5 string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE md5 = ?)", md5).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("sqlite store existsbymd5: %w", err)
	}
	return exists != 0, nil
}

// ShiftSortKey reorders a job to a new integer 0-based index position.
func (s *SQLiteStore) ShiftSortKey(ctx context.Context, id string, newIndex int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite store begin tx shift: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs").Scan(&n); err != nil || n <= 1 {
		return nil
	}
	if newIndex < 0 {
		newIndex = 0
	}
	if newIndex >= n {
		newIndex = n - 1
	}

	var oldIndex int
	err = tx.QueryRowContext(ctx, "SELECT sort_key FROM jobs WHERE id = ?", id).Scan(&oldIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil || oldIndex == newIndex {
		return nil
	}

	if newIndex < oldIndex {
		_, _ = tx.ExecContext(ctx, "UPDATE jobs SET sort_key = -1 WHERE id = ?", id)
		_, err = tx.ExecContext(ctx, "UPDATE jobs SET sort_key = sort_key + 1 WHERE sort_key >= ? AND sort_key < ?", newIndex, oldIndex)
		_, _ = tx.ExecContext(ctx, "UPDATE jobs SET sort_key = ? WHERE id = ?", newIndex, id)
	} else {
		_, _ = tx.ExecContext(ctx, "UPDATE jobs SET sort_key = -1 WHERE id = ?", id)
		_, err = tx.ExecContext(ctx, "UPDATE jobs SET sort_key = sort_key - 1 WHERE sort_key > ? AND sort_key <= ?", oldIndex, newIndex)
		_, _ = tx.ExecContext(ctx, "UPDATE jobs SET sort_key = ? WHERE id = ?", newIndex, id)
	}
	if err != nil {
		return fmt.Errorf("sqlite store shift range: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite store commit shift %s: %w", id, err)
	}
	return nil
}

// Prune removes orphaned filesystem files not present in SQLite.
func (s *SQLiteStore) Prune(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM jobs")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	active := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			active[id] = true
		}
	}
	_ = rows.Err()

	cleanDir := func(subdir string) {
		dirPath := filepath.Join(s.dir, subdir)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			return
		}
		for _, e := range entries {
			if before, ok := strings.CutSuffix(e.Name(), ".json.gz"); ok {
				id := before
				if !active[id] {
					_ = os.Remove(filepath.Join(dirPath, e.Name()))
				}
			}
		}
	}

	cleanDir("manifests")
	cleanDir("progress")
	cleanDir("jobs")
	_ = os.Remove(filepath.Join(s.dir, "queue.json.gz"))
	return nil
}

// SetPaused sets the global queue paused state in queue_meta.
func (s *SQLiteStore) SetPaused(ctx context.Context, paused bool) error {
	val := "false"
	if paused {
		val = "true"
	}
	const q = "INSERT INTO queue_meta (key, value) VALUES ('paused', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value"
	_, err := s.db.ExecContext(ctx, q, val)
	return err
}

// IsPaused reports whether the global queue is paused.
func (s *SQLiteStore) IsPaused(ctx context.Context) (bool, error) {
	var val string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM queue_meta WHERE key = 'paused'").Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return val == "true", nil
}
