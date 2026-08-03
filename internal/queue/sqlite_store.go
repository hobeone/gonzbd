package queue

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	// log is captured once at construction and component-scoped, matching
	// Queue's own logger. docs/go-standards.md forbids reaching for a
	// package-level global at each call site.
	log *slog.Logger
}

// NewSQLiteStore constructs a SQLiteStore backed by db and storing manifests inside dir.
func NewSQLiteStore(db *sql.DB, dir string, historyRepo *history.Repository) *SQLiteStore {
	return &SQLiteStore{
		db:          db,
		dir:         dir,
		historyRepo: historyRepo,
		log:         slog.Default().With("component", "queue.store"),
	}
}

// Dir returns the persistent root directory managed by the store.
func (s *SQLiteStore) Dir() string {
	return s.dir
}

func (s *SQLiteStore) resequenceTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM jobs ORDER BY sort_key ASC, time_added ASC")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET sort_key = ? WHERE id = ?", i, id); err != nil {
			return err
		}
	}
	return nil
}

// encodeArticlesDone packs file fileIdx's per-article done+failed state into
// a hex bitmap for job_files.articles_done. The encoding is two
// equal-length bitmaps concatenated — [done bits][failed bits], each
// ceil(N/8) bytes for N articles in the file — so a failed article (done &&
// failed) round-trips distinctly from a plain successful one (done &&
// !failed). This widens the historical done-only format (see
// decodeArticlesDone for the backward-compat read path); the column stays
// an opaque hex string with no schema change.
func encodeArticlesDone(job *Job, fileIdx int) string {
	if job == nil || job.Progress() == nil {
		return ""
	}
	m, err := job.Manifest()
	if err != nil || fileIdx < 0 || fileIdx >= m.NumFiles() {
		return ""
	}
	lo, hi := m.FileRange(fileIdx)
	if hi <= lo {
		return ""
	}
	n := hi - lo
	numBytes := (n + 7) / 8
	buf := make([]byte, numBytes*2)
	for i := range n {
		if job.Progress().ArticleDone(lo + i) {
			buf[i/8] |= 1 << (i % 8)
		}
		if job.Progress().ArticleFailed(lo + i) {
			buf[numBytes+i/8] |= 1 << (i % 8)
		}
	}
	return hex.EncodeToString(buf)
}

// decodeArticlesDone restores file fileIdx's per-article done/failed state
// from a hex bitmap written by encodeArticlesDone: [done bits][failed bits],
// each ceil(N/8) bytes. An article with its failed bit set is restored via
// markFailed, a plain done article via markDone — never both, since
// markFailed early-returns once done[i] is already true.
//
// Any other length is corrupt and restores nothing. Sizes are fixed by the
// file's article count, so a mismatch means the value did not come from
// encodeArticlesDone for this file, and guessing which prefix is the done
// bitmap would restore the wrong articles rather than none.
func (s *SQLiteStore) decodeArticlesDone(encoded string, job *Job, fileIdx int) {
	if job == nil || job.Progress() == nil || encoded == "" {
		return
	}
	m, mErr := job.Manifest()
	if mErr != nil || fileIdx < 0 || fileIdx >= m.NumFiles() {
		return
	}
	buf, err := hex.DecodeString(encoded)
	if err != nil {
		return
	}
	lo, hi := m.FileRange(fileIdx)
	if hi <= lo {
		return
	}
	n := hi - lo
	numBytes := (n + 7) / 8
	if len(buf) != numBytes*2 {
		s.log.Warn("articles_done bitmap has an unexpected length, per-article state not restored",
			"job_id", job.ID, "file_index", fileIdx, "want_bytes", numBytes*2, "got_bytes", len(buf))
		return
	}
	doneBuf, failedBuf := buf[:numBytes], buf[numBytes:]

	for i := range n {
		switch {
		case failedBuf[i/8]&(1<<(i%8)) != 0:
			job.progress.markFailed(job.manifest, lo+i)
		case doneBuf[i/8]&(1<<(i%8)) != 0:
			job.progress.markDone(job.manifest, lo+i)
		}
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

	var dlStartedUnix, dlFinishedUnix int64
	if job.Progress() != nil {
		if !job.Progress().DownloadStarted().IsZero() {
			dlStartedUnix = job.Progress().DownloadStarted().Unix()
		}
		if !job.Progress().DownloadFinished().IsZero() {
			dlFinishedUnix = job.Progress().DownloadFinished().Unix()
		}
	}

	const qJobs = `
INSERT INTO jobs
  (id, filename, name, password, url, category, priority, status, pp, script,
   time_added, md5, avg_age, groups, meta, warning, postproc, sort_key,
   download_started, download_finished, par2_bytes, par2_files, nzb_backup)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	postprocInt := 0
	if job.PostProc {
		postprocInt = 1
	}

	_, err = tx.ExecContext(ctx, qJobs,
		job.ID, job.Filename, job.Name, job.Password, job.URL, job.Category,
		int(job.Priority), string(job.Status), job.PP, job.Script,
		job.Added.Unix(), job.MD5, job.AvgAge.Unix(), string(groupsJSON), string(metaJSON),
		job.Warning, postprocInt, sortKey, dlStartedUnix, dlFinishedUnix,
		// From the promoted scalars rather than job.Manifest(): they are set
		// at Add and stay correct while the manifest is evicted, so this
		// writes the right values even for a job persisted while non-resident.
		job.Par2Bytes(), job.Par2Files(),
		job.NZBBackup,
	)
	if err != nil {
		return fmt.Errorf("sqlite store insert job %s: %w", job.ID, err)
	}

	if m, mErr := job.Manifest(); mErr == nil {
		const qFiles = `
INSERT INTO job_files
  (job_id, file_index, subject, date, bytes, is_par2_recovery, complete, deferred, write_cursor, bytes_downloaded, filename, assembled_crc32, articles_done, article_count)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		p := job.Progress()
		for i := range m.NumFiles() {
			isPar2 := 0
			if m.FileIsPar2Recovery(i) {
				isPar2 = 1
			}
			// complete and deferred come from the job, not from literal
			// zeros. Hard-coding them discarded on-demand par2's whole
			// effect (#287): NewJob defers each recovery volume in
			// JobProgress, this INSERT wrote deferred = 0, and the first
			// promotion read that back over the live flag — so a feature
			// whose entire purpose is to not download those volumes
			// downloaded them, in every configuration with a store.
			complete, deferred := 0, 0
			if p.FileComplete(i) {
				complete = 1
			}
			if p.FileDeferred(i) {
				deferred = 1
			}
			artDoneStr := encodeArticlesDone(job, i)
			lo, hi := m.FileRange(i)
			_, err = tx.ExecContext(ctx, qFiles,
				job.ID, i, m.FileSubject(i), m.FileDate(i).Unix(), m.FileBytes(i), isPar2,
				complete, deferred,
				p.FileWriteCursor(i), p.FileBytesDownloaded(i), p.FileFilename(i), p.FileAssembledCRC32(i),
				artDoneStr, hi-lo,
			)
			if err != nil {
				return fmt.Errorf("sqlite store insert job_file %s/%d: %w", job.ID, i, err)
			}
		}

		manifestPath := filepath.Join(s.dir, "manifests", job.ID+".json.gz")
		if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
			return fmt.Errorf("sqlite store mkdir manifests: %w", err)
		}
		if err := writeGzJSON(manifestPath, m); err != nil {
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
SELECT id, filename, name, COALESCE(password, ''), COALESCE(url, ''), COALESCE(category, ''), priority, status, pp, COALESCE(script, ''),
       time_added, md5, avg_age, COALESCE(groups, ''), COALESCE(meta, ''), COALESCE(warning, ''), postproc,
       download_started, download_finished, par2_bytes, par2_files,
       COALESCE(nzb_backup, '')
FROM jobs WHERE id = ?`

	var job Job
	var groupsStr, metaStr, statusStr string
	var priorityInt, ppInt, postprocInt, par2Files int
	var addedUnix, avgAgeUnix, dlStartedUnix, dlFinishedUnix, par2Bytes int64

	err := s.db.QueryRowContext(ctx, qJob, id).Scan(
		&job.ID, &job.Filename, &job.Name, &job.Password, &job.URL, &job.Category,
		&priorityInt, &statusStr, &ppInt, &job.Script, &addedUnix, &job.MD5, &avgAgeUnix,
		&groupsStr, &metaStr, &job.Warning, &postprocInt,
		&dlStartedUnix, &dlFinishedUnix, &par2Bytes, &par2Files,
		&job.NZBBackup,
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

	var fileCount int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM job_files WHERE job_id = ?", id).Scan(&fileCount); err != nil {
		_ = s.Remove(ctx, id)
		return nil, fmt.Errorf("sqlite store count files for %s: %w", id, err)
	}

	manifestPath := filepath.Join(s.dir, "manifests", id+".json.gz")
	if fileCount > 0 {
		if _, err := os.Stat(manifestPath); err != nil {
			_ = s.Remove(ctx, id)
			return nil, fmt.Errorf("sqlite store manifest missing for %s: %w", id, err)
		}
	}

	if isResidentStatus(job.Status) {
		if _, err := os.Stat(manifestPath); err == nil {
			var manifest Manifest
			if err := readGzJSON(manifestPath, &manifest); err != nil {
				_ = os.Rename(manifestPath, manifestPath+".corrupt")
				_ = s.Remove(ctx, id)
				return nil, fmt.Errorf("sqlite store read manifest %s: %w", id, err)
			}
			job.manifest = &manifest
			job.setScalarsFromManifest(&manifest)
			job.progress = newJobProgress(&manifest)
			if dlStartedUnix > 0 {
				job.progress.downloadStarted = time.Unix(dlStartedUnix, 0).UTC()
			}
			if dlFinishedUnix > 0 {
				job.progress.downloadFinished = time.Unix(dlFinishedUnix, 0).UTC()
			}
			_ = s.RestoreJobProgress(ctx, &job)
		}
	}

	// job.manifest is nil here for a non-resident job (or a resident one
	// whose manifest file is missing, though that case already errored
	// above when fileCount > 0). Without a resident manifest,
	// TotalBytes/NumFiles/NumArticles would otherwise read as zero — a
	// gap left open by Task 3 — so reconstruct the three of the five
	// scalars that job_files can answer on its own. par2Bytes/par2Files
	// stay zero; see setAggregateScalarsFromFiles for why they cannot be
	// safely reconstructed from is_par2_recovery.
	//
	// A query failure here is logged rather than failing Get outright: the
	// file-count query above fails closed because its result gates whether
	// the caller gets a job back at all (admission), but this query only
	// feeds three reporting scalars for a job that is otherwise valid and
	// already fully loaded. Returning the job with those three at zero (its
	// pre-Task-4 behavior) is preferable to losing the job entirely over a
	// transient SQLite error on an already-slow, non-resident read path —
	// but the error must not vanish silently, so it goes to the log.
	// par2 comes from the job row, not from job_files, and is set
	// independently of the aggregate query below so a failure there cannot
	// take it down with the other three. A resident job already has the
	// authoritative values from setScalarsFromManifest above.
	if job.manifest == nil {
		job.setPar2ScalarsFromStore(par2Bytes, par2Files)
	}

	if job.manifest == nil && fileCount > 0 {
		var totalBytes int64
		var numFiles, numArticles int
		const qAgg = `SELECT COALESCE(SUM(bytes), 0), COUNT(*), COALESCE(SUM(article_count), 0) FROM job_files WHERE job_id = ?`
		if err := s.db.QueryRowContext(ctx, qAgg, id).Scan(&totalBytes, &numFiles, &numArticles); err != nil {
			s.log.Error("aggregate scalars from job_files failed, reporting scalars stay zero",
				"job_id", id, "err", err)
		} else {
			job.setAggregateScalarsFromFiles(totalBytes, numFiles, numArticles)
		}
	}

	return &job, nil
}

func isResidentStatus(status constants.Status) bool {
	j := Job{Status: status}
	return j.Phase().IsResident()
}

// RestoreJobProgress loads per-file progress counters from SQLite into job.progress for a resident job.
func (s *SQLiteStore) RestoreJobProgress(ctx context.Context, job *Job) error {
	if job == nil || job.manifest == nil || job.progress == nil {
		return nil
	}
	const qFiles = `
SELECT file_index, complete, deferred, write_cursor, bytes_downloaded, assembled_crc32, COALESCE(articles_done, '')
FROM job_files WHERE job_id = ? ORDER BY file_index ASC`
	rows, err := s.db.QueryContext(ctx, qFiles, job.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var idx, complete, deferred int
		var writeCursor, bytesDownloaded int64
		var crc32Val uint32
		var artDoneStr string
		if err := rows.Scan(&idx, &complete, &deferred, &writeCursor, &bytesDownloaded, &crc32Val, &artDoneStr); err != nil {
			return fmt.Errorf("sqlite store scan job_file for %s: %w", job.ID, err)
		}
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
			} else if artDoneStr != "" {
				s.decodeArticlesDone(artDoneStr, job, idx)
			}
			if deferred != 0 {
				fp.Deferred = true
			}
		}
	}
	job.progress.recompute(job.manifest)
	return rows.Err()
}

// ArticleCountsByJob returns every job's per-file article counts in a single
// grouped query, indexed by file_index within each job. Used to size
// JobProgress at restart without loading each job's manifest individually —
// collapsed from a per-job query (the RestoreJobProgress-adjacent shape
// RemainingBytesByJob already uses) so that a large queued backlog costs one
// round trip instead of N.
//
// Counts are placed by file_index rather than by scan order, so a job whose
// job_files rows have non-contiguous indices (e.g. after a partial delete)
// still gets each count attributed to the right file instead of shifted by
// position.
func (s *SQLiteStore) ArticleCountsByJob(ctx context.Context) (map[string][]int, error) {
	const q = `SELECT job_id, file_index, article_count FROM job_files ORDER BY job_id ASC, file_index ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sqlite store article counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string][]int)
	for rows.Next() {
		var jobID string
		var idx, count int
		if err := rows.Scan(&jobID, &idx, &count); err != nil {
			return nil, fmt.Errorf("sqlite store scan article count: %w", err)
		}
		if idx < 0 {
			// file_index is only ever written from a loop index, so this is
			// DB-corruption-only, but RestoreJobProgress guards the same
			// column with idx >= 0 a few lines up — match it here too rather
			// than let a corrupt row panic the boot path below.
			continue
		}
		counts := result[jobID]
		if idx >= len(counts) {
			grown := make([]int, idx+1)
			copy(grown, counts)
			counts = grown
		}
		counts[idx] = count
		result[jobID] = counts
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite store article counts: %w", err)
	}
	return result, nil
}

// RemainingBytesByJob returns each job's remaining bytes (bytes minus
// bytes_downloaded, summed per job_id) across job_files. Unlike
// RestoreJobProgress, this needs no resident manifest/progress: it reads
// straight from the persisted per-file counters, which is what makes it
// usable to seed JobProgress.remainingBytes for non-resident jobs during
// Load.
func (s *SQLiteStore) RemainingBytesByJob(ctx context.Context) (map[string]int64, error) {
	const q = `SELECT job_id, SUM(bytes - bytes_downloaded) FROM job_files GROUP BY job_id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sqlite store remaining bytes by job: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]int64)
	for rows.Next() {
		var jobID string
		var remaining int64
		if err := rows.Scan(&jobID, &remaining); err != nil {
			return nil, fmt.Errorf("sqlite store scan remaining bytes: %w", err)
		}
		if remaining < 0 {
			// Shouldn't happen (bytes_downloaded can't exceed a file's byte
			// count in normal operation), but a negative "remaining" figure
			// would be nonsensical to report to the UI, so clamp rather
			// than propagate it.
			remaining = 0
		}
		result[jobID] = remaining
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite store remaining bytes by job: %w", err)
	}
	return result, nil
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

// sqlExecer is satisfied by both *sql.DB and *sql.Tx, letting updateTx run
// either as a standalone statement (Update) or inside a caller-managed
// transaction (UpdateBatch).
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// updateTx performs the jobs/job_files UPDATE statements for a single job
// against execer. Shared by Update (single-statement) and UpdateBatch
// (one shared transaction) so the SQL lives in exactly one place.
func (s *SQLiteStore) updateTx(ctx context.Context, execer sqlExecer, job *Job) error {
	const q = `
UPDATE jobs SET
  name = ?, category = ?, priority = ?, status = ?, pp = ?, script = ?, warning = ?, postproc = ?,
  download_started = ?, download_finished = ?
WHERE id = ?`

	postprocInt := 0
	if job.PostProc {
		postprocInt = 1
	}

	var dlStartedUnix, dlFinishedUnix int64
	if job.Progress() != nil {
		if !job.Progress().DownloadStarted().IsZero() {
			dlStartedUnix = job.Progress().DownloadStarted().Unix()
		}
		if !job.Progress().DownloadFinished().IsZero() {
			dlFinishedUnix = job.Progress().DownloadFinished().Unix()
		}
	}

	res, err := execer.ExecContext(ctx, q,
		job.Name, job.Category, int(job.Priority), string(job.Status), job.PP, job.Script, job.Warning, postprocInt, dlStartedUnix, dlFinishedUnix, job.ID,
	)
	if err != nil {
		return fmt.Errorf("sqlite store update job %s: %w", job.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}

	if m, mErr := job.Manifest(); job.Progress() != nil && mErr == nil {
		// article_count is included here (not just in Add's INSERT) so that a
		// row written before this column existed — where it defaults to 0 —
		// gets backfilled the next time the job is updated while resident and
		// its manifest is available, rather than staying 0 forever.
		const qF = `UPDATE job_files SET complete = ?, deferred = ?, write_cursor = ?, bytes_downloaded = ?, filename = ?, assembled_crc32 = ?, articles_done = ?, article_count = ? WHERE job_id = ? AND file_index = ?`
		for i := range m.NumFiles() {
			complete := 0
			if job.Progress().FileComplete(i) {
				complete = 1
			}
			deferred := 0
			if job.Progress().FileDeferred(i) {
				deferred = 1
			}
			artDoneStr := encodeArticlesDone(job, i)
			lo, hi := m.FileRange(i)
			if _, err := execer.ExecContext(ctx, qF,
				complete, deferred, job.Progress().FileWriteCursor(i), job.Progress().FileBytesDownloaded(i), job.Progress().FileFilename(i), job.Progress().FileAssembledCRC32(i), artDoneStr, hi-lo,
				job.ID, i,
			); err != nil {
				return fmt.Errorf("sqlite store update job_file %s index %d: %w", job.ID, i, err)
			}
		}
	}

	return nil
}

// Update modifies an existing active job's live metadata in SQLite.
func (s *SQLiteStore) Update(ctx context.Context, job *Job) error {
	return s.updateTx(ctx, s.db, job)
}

// UpdateBatch atomically persists jobs in a single SQLite transaction: either
// all updates are committed or, on any failure, none are.
func (s *SQLiteStore) UpdateBatch(ctx context.Context, jobs []*Job) error {
	if len(jobs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite store begin tx updatebatch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, job := range jobs {
		if err := s.updateTx(ctx, tx, job); err != nil {
			return fmt.Errorf("sqlite store updatebatch %s: %w", job.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite store commit updatebatch: %w", err)
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

	_ = s.resequenceTx(ctx, tx)

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite store commit remove %s: %w", id, err)
	}

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

	// A failed job keeps its per-file progress so a retry can refetch only
	// the articles that did not make it — including the case where every
	// article is present and only post-processing failed, which must not
	// re-download anything. A completed job has nothing to retry, and
	// retaining for every job is what made the payload format this replaces
	// grow without bound. Same transaction as the delete below, so the rows
	// are either carried over or not written at all.
	if entry.Status == string(constants.StatusFailed) {
		const qRetain = `
INSERT INTO history_job_files
  (job_id, file_index, complete, deferred, write_cursor, bytes_downloaded, filename, assembled_crc32, articles_done, article_count)
SELECT job_id, file_index, complete, deferred, write_cursor, bytes_downloaded, filename, assembled_crc32, articles_done, article_count
FROM job_files WHERE job_id = ?`
		if _, err := tx.ExecContext(ctx, qRetain, job.ID); err != nil {
			return fmt.Errorf("sqlite store retain job_files %s: %w", job.ID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM job_files WHERE job_id = ?", job.ID); err != nil {
		return fmt.Errorf("sqlite store delete job_files %s: %w", job.ID, err)
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", job.ID)
	if err != nil {
		return fmt.Errorf("sqlite store delete active job %s: %w", job.ID, err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrNotFound
	}

	_ = s.resequenceTx(ctx, tx)

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite store commit movetohistory %s: %w", job.ID, err)
	}

	return nil
}

// RetainedFile is one file's download progress, kept for a failed job so a
// retry can skip the articles that already succeeded.
//
// ArticleCount is not part of that progress. It is the alignment check: a
// retry rebuilds the manifest by re-parsing the NZB, and ArticlesDone is a
// bitmap indexed against the manifest that produced it. Applying it to a
// manifest with a different shape would mark the wrong articles done and
// silently skip real downloads, so the counts must match before the overlay
// is used.
type RetainedFile struct {
	FileIndex       int
	Complete        bool
	Deferred        bool
	WriteCursor     int64
	BytesDownloaded int64
	Filename        string
	AssembledCRC32  uint32
	ArticlesDone    string
	ArticleCount    int
}

// HistoryFileProgress returns the per-file progress retained for a failed
// history job, ordered by file index. An empty slice means nothing was
// retained — the job succeeded, its entry predates retention, or its rows
// were already deleted — and is not an error.
func (s *SQLiteStore) HistoryFileProgress(ctx context.Context, jobID string) ([]RetainedFile, error) {
	const q = `
SELECT file_index, complete, deferred, write_cursor, bytes_downloaded,
       COALESCE(filename, ''), COALESCE(assembled_crc32, 0), COALESCE(articles_done, ''), article_count
FROM history_job_files WHERE job_id = ? ORDER BY file_index ASC`
	rows, err := s.db.QueryContext(ctx, q, jobID)
	if err != nil {
		return nil, fmt.Errorf("sqlite store history file progress %s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []RetainedFile
	for rows.Next() {
		var f RetainedFile
		var complete, deferred int
		if err := rows.Scan(&f.FileIndex, &complete, &deferred, &f.WriteCursor,
			&f.BytesDownloaded, &f.Filename, &f.AssembledCRC32, &f.ArticlesDone, &f.ArticleCount); err != nil {
			return nil, fmt.Errorf("sqlite store scan history_job_file %s: %w", jobID, err)
		}
		f.Complete = complete != 0
		f.Deferred = deferred != 0
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite store history file progress rows %s: %w", jobID, err)
	}
	return out, nil
}

// DeleteJobArtifacts removes the on-disk manifest and progress files for job
// id. See the Store interface doc comment for the ordering requirement this
// depends on and why deletion is best-effort.
func (s *SQLiteStore) DeleteJobArtifacts(_ context.Context, id string) error {
	var errs []error
	if err := os.Remove(filepath.Join(s.dir, "manifests", id+".json.gz")); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove manifest: %w", err))
	}
	if err := os.Remove(filepath.Join(s.dir, "progress", id+".json.gz")); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove progress: %w", err))
	}
	return errors.Join(errs...)
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

	rows, err := tx.QueryContext(ctx, "SELECT id FROM jobs ORDER BY sort_key ASC, time_added ASC")
	if err != nil {
		return fmt.Errorf("sqlite store shift query: %w", err)
	}
	var ids []string
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err == nil {
			ids = append(ids, jobID)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite store shift scan: %w", err)
	}

	oldIndex := -1
	for i, jobID := range ids {
		if jobID == id {
			oldIndex = i
			break
		}
	}
	if oldIndex == -1 {
		return ErrNotFound
	}

	n := len(ids)
	if n <= 1 || oldIndex == newIndex {
		return nil
	}

	ids = slices.Delete(ids, oldIndex, oldIndex+1)
	ids = slices.Insert(ids, newIndex, id)

	for i, jobID := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET sort_key = ? WHERE id = ?", i, jobID); err != nil {
			return fmt.Errorf("sqlite store shift update %s: %w", jobID, err)
		}
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
