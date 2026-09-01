package queue

import (
	"context"
	"database/sql"
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

// encodeJobStamp is the wire form of a download timestamp: 0 means absent.
//
// The sentinel is unambiguous because isJobStamp — the owner's bound in
// progress.go — is defined on t.Unix() > 0, the same quantity this writes. Any
// stamp the owner accepts encodes to a positive integer, and no stamp it
// accepts encodes to 0. That equivalence is what let #464 close without making
// the columns nullable, and
// TestJobStampCodec_RoundTripsAndAgreesWithTheOwnersBound asserts it rather
// than trusting this sentence.
//
// It exists because addTx and updateTx carried byte-identical copies of this
// conversion — issue #464's option 3, worth doing whether or not the epoch bug
// existed. Those copies also read the field through IsZero, which is precisely
// the test that admits time.Unix(0,0) and encodes it to the absent sentinel.
func encodeJobStamp(t time.Time) int64 {
	if !isJobStamp(t) {
		return 0
	}
	return t.Unix()
}

// decodeJobStamp inverts encodeJobStamp.
func decodeJobStamp(unix int64) time.Time {
	if unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0).UTC()
}

// DownloadStamps carries one job's two persisted download timestamps, already
// decoded. Either may be the zero time, meaning the column held the absent
// sentinel.
type DownloadStamps struct {
	Started  time.Time
	Finished time.Time
}

// DownloadStampsByJob implements Store.
func (s *SQLiteStore) DownloadStampsByJob(ctx context.Context) (map[string]DownloadStamps, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, download_started, download_finished FROM jobs")
	if err != nil {
		return nil, fmt.Errorf("sqlite store download stamps query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]DownloadStamps)
	for rows.Next() {
		var id string
		var startedUnix, finishedUnix int64
		if err := rows.Scan(&id, &startedUnix, &finishedUnix); err != nil {
			return nil, fmt.Errorf("sqlite store download stamps scan: %w", err)
		}
		out[id] = DownloadStamps{
			Started:  decodeJobStamp(startedUnix),
			Finished: decodeJobStamp(finishedUnix),
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite store download stamps rows: %w", err)
	}
	return out, nil
}

// Dir returns the persistent root directory managed by the store.
func (s *SQLiteStore) Dir() string {
	return s.dir
}

// withWriteTx runs fn inside a write transaction, committing on success and
// rolling back on any error.
//
// It deliberately does not retry. An earlier version did, on the theory that
// a contended transaction should be restarted, and that was wrong twice over
// once _txlock=immediate landed in internal/history/db.go:
//
//   - It buys nothing. Taking the write lock at BEGIN means contention is an
//     ordinary wait that busy_timeout(5000) already performs, so a failure
//     here is a transaction that waited five seconds and still lost. Measured
//     against an unthrottled competing writer, a single attempt succeeds as
//     reliably as ten (0 failures in 20 runs of
//     TestSQLiteStore_AddSurvivesConcurrentCommits either way).
//   - It costs a great deal. Contention now surfaces at BeginTx, which blocks
//     for the full busy_timeout rather than failing fast — measured at 5.01s —
//     so retrying multiplied the wait rather than avoiding it: ten attempts
//     took 48.8s to fail. Queue.Add calls store.Add and store.ShiftSortKey
//     back to back inside q.mu, so that stall would have frozen every other
//     queue operation for the duration.
//
// The bounded five-second wait busy_timeout provides is the whole retry
// policy, and it lives in one place rather than being multiplied here.
func (s *SQLiteStore) withWriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite store begin tx: %w", err)
	}
	err = fn(tx)
	if err == nil {
		err = tx.Commit()
	}
	// Safe after a successful Commit: Rollback then returns ErrTxDone,
	// which is exactly the "already finished" signal, not a failure.
	_ = tx.Rollback()
	return err
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

// artRange is one durable_runs row's article span, [First, Last] inclusive
// and indexed GLOBALLY within the job's manifest.
//
// Only the two index columns, because that is all article resolution needs: a
// run's offset, length and CRC answer the truncate bound, the overlap check
// and the whole-file CRC, and none of those questions is asked here.
type artRange struct{ First, Last int32 }

// resolveArticles derives one job's per-article done/failed state from its
// durable runs and its permanently-failed articles.
//
// This is what replaced job_files.articles_done, and the shape of the
// replacement is the point: resolution is DERIVED from the two records that
// already hold it, rather than re-serialised into a third copy on every job
// update. done means "covered by a run", failed means "has a failed_articles
// row", and neither means the article is still outstanding.
//
// failed implies done, and both callers depend on it. Each reads Failed only
// inside the Done branch — JobProgress.markFailed early-returns once done is
// set, and newJobProgressSized nests the two the same way — so a failed
// article that did not also read as done would come back Pending and be
// fetched again on every restart.
//
// A failed article is never covered by a run: it never decoded, so nothing was
// ever written for it and no fsync could have recorded it. The done bit it
// gets here is this function's, not a run's.
//
// Both returned slices are indexed globally and are numArticles long.
func resolveArticles(runs []artRange, failed []int32, numArticles int) (done, failedOut []bool) {
	done = make([]bool, numArticles)
	failedOut = make([]bool, numArticles)
	for _, r := range runs {
		// A row naming an article outside the manifest is clamped rather than
		// trusted. It means the manifest was rebuilt to a different shape
		// under rows keyed on the old numbering — the condition
		// RetryHistoryJob decides and drops the rows for, loudly. Clamping
		// here keeps a row that escaped that from indexing out of bounds.
		lo, hi := max(int(r.First), 0), min(int(r.Last), numArticles-1)
		for i := lo; i <= hi; i++ {
			done[i] = true
		}
	}
	for _, idx := range failed {
		i := int(idx)
		if i < 0 || i >= numArticles {
			continue
		}
		done[i] = true
		failedOut[i] = true
	}
	return done, failedOut
}

// runRangesForJob reads one job's durable_runs article spans.
func (s *SQLiteStore) runRangesForJob(ctx context.Context, jobID string) ([]artRange, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT first_art_idx, last_art_idx FROM durable_runs WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, fmt.Errorf("sqlite store durable runs %s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []artRange
	for rows.Next() {
		var r artRange
		if err := rows.Scan(&r.First, &r.Last); err != nil {
			return nil, fmt.Errorf("sqlite store scan durable run %s: %w", jobID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// failedArticlesForJob reads one job's permanently-failed article indices.
func (s *SQLiteStore) failedArticlesForJob(ctx context.Context, jobID string) ([]int32, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT art_idx FROM failed_articles WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, fmt.Errorf("sqlite store failed articles %s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []int32
	for rows.Next() {
		var idx int32
		if err := rows.Scan(&idx); err != nil {
			return nil, fmt.Errorf("sqlite store scan failed article %s: %w", jobID, err)
		}
		out = append(out, idx)
	}
	return out, rows.Err()
}

// resolutionForJob derives one job's global done/failed slices from stable
// storage.
//
// numArticles comes from the caller because the two callers know it
// differently: a resident job asks its manifest, and the boot path sums
// job_files.article_count.
func (s *SQLiteStore) resolutionForJob(ctx context.Context, jobID string, numArticles int) (done, failed []bool, err error) {
	runs, err := s.runRangesForJob(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	failedIdx, err := s.failedArticlesForJob(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	d, f := resolveArticles(runs, failedIdx, numArticles)
	return d, f, nil
}

// RecordFailedArticles persists articles that will never be fetched.
//
// INSERT OR IGNORE because AckPermanentFailure is reachable more than once for
// the same article and a repeat is a no-op rather than an update — the row
// carries no value beyond its own existence.
//
// Its reversal is deliberately NOT per-article; see ClearFailedArticles.
func (s *SQLiteStore) RecordFailedArticles(ctx context.Context, jobID string, artIdxs []int32) error {
	if len(artIdxs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite store begin failed articles %s: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO failed_articles (job_id, art_idx) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("sqlite store prepare failed articles %s: %w", jobID, err)
	}
	defer func() { _ = stmt.Close() }()
	for _, idx := range artIdxs {
		if _, err := stmt.ExecContext(ctx, jobID, idx); err != nil {
			return fmt.Errorf("sqlite store record failed article %s/%d: %w", jobID, idx, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite store commit failed articles %s: %w", jobID, err)
	}
	return nil
}

// ClearFailedArticles removes every failed-article row for ONE job.
//
// A whole-job delete rather than a per-article one, because that is what its
// callers actually do: Job.ResetForRetry clears precisely the articles whose
// failed bit is set, which is precisely the set stored here. Batching is
// therefore exact rather than approximate.
//
// Three callers, enumerated from `git grep -n ClearFailedArticles`. Queue.Retry
// is in this package. Application.RetryHistoryJob calls ResetForRetry on a job
// rebuilt from its NZB backup, outside the queue and before Queue.Add, and so
// calls this method directly; its sibling branch reaches the rows through
// Application.dropJobDurability instead, so both of its paths are covered but
// by different routes. Application.dropJobDurability is the third, and it is
// not a reversal — it drops the rows of a DEPARTING job, where no in-memory
// state survives for them to disagree with.
//
// Queue.ClearAllEmitted was a third caller until #426 and is not one now. Its
// per-article reset stopped being exhaustive — JobProgress.resetForReload
// retains the failed bit of an article whose file is already Complete — so the
// equivalence above no longer holds for it and a whole-job delete would drop
// rows for articles that are still failed in memory. It uses
// ClearFailedArticlesByIdx.
//
// The SCOPE is what makes the equivalence true where it does hold: this must
// be called for a job the caller is actually resetting. A delete that swept
// every job would resurrect the permanently failed articles of every
// NON-resident job as fetchable work, and the symptom is a silent re-download
// storm rather than an error.
//
// This reversal has to exist at all only because the record is now a table.
// job_files.articles_done was re-serialised wholesale on the next store
// update, so a cleared bit corrected itself; a separate table has no wholesale
// rewrite, and a stale row would survive a retry and make the next restart
// mark the retry's re-fetched articles failed.
func (s *SQLiteStore) ClearFailedArticles(ctx context.Context, jobID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM failed_articles WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("sqlite store clear failed articles %s: %w", jobID, err)
	}
	return nil
}

// ClearFailedArticlesByIdx removes the failed-article rows for exactly artIdxs
// within jobID. See the Store interface for why this form exists.
//
// One transaction, so a caller that resets N articles never observes a partial
// reversal: the alternative leaves memory and the record disagreeing about a
// subset nobody can name afterwards.
//
// Chunked to the same 999-host-parameter budget history.Repository.Delete
// works to, but at 998 indices per statement rather than 999: jobID is bound
// too, and it is the total that the limit applies to. The chunking is why this
// enumerates the rows to REMOVE rather than the rows to keep — a "delete
// everything except these" form is not chunkable, because the second
// statement's NOT IN would delete what the first was preserving.
func (s *SQLiteStore) ClearFailedArticlesByIdx(ctx context.Context, jobID string, artIdxs []int32) error {
	if len(artIdxs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite store begin clear failed articles %s: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback() }()

	const chunkSize = 998 // 999 SQLite host parameters, less the one jobID binds
	for i := 0; i < len(artIdxs); i += chunkSize {
		chunk := artIdxs[i:min(i+chunkSize, len(artIdxs))]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, jobID)
		for _, idx := range chunk {
			args = append(args, idx)
		}
		// The only thing concatenated is a run of "?" placeholders whose
		// length comes from len(chunk); every artIdx is bound, never
		// formatted in. A variable-length IN list has no placeholder-free
		// form, which is why the same exemption sits on
		// history.Repository.Search's WHERE assembly.
		placeholders := strings.Repeat(",?", len(chunk)-1)
		query := `DELETE FROM failed_articles WHERE job_id = ? AND art_idx IN (?` + placeholders + `)` //nolint:gosec // G202: bound placeholders only, no caller data reaches the string
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("sqlite store clear failed articles by idx %s: %w", jobID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite store commit clear failed articles by idx %s: %w", jobID, err)
	}
	return nil
}

// applyResolution installs a derived done/failed pair onto a resident job's
// progress, over one file's global article range [lo, hi).
//
// markFailed before markDone, and never both: markFailed early-returns once
// the article is already done, so the ORDER is what keeps a permanently failed
// article from being restored as a successful one. That inversion is #300 — a
// restart rewrote failed articles as successful, JobProgress.recompute derived
// failedBytes from the bits, and the job came back reporting full health while
// a retry found nothing to refetch.
//
// There is deliberately no fallback for an empty resolution. Defaulting to
// "all done" is the same override in miniature, and in the safe direction an
// unknown article is one to fetch again, not one to skip.
func applyResolution(job *Job, done, failed []bool, lo, hi int) {
	for i := lo; i < hi; i++ {
		switch {
		case i < len(failed) && failed[i]:
			job.progress.markFailed(job.manifest, i)
		case i < len(done) && done[i]:
			job.progress.markDone(job.manifest, i)
		}
	}
}

// Add inserts a new active job into the store and writes its manifest to disk.
func (s *SQLiteStore) Add(ctx context.Context, job *Job) error {
	m, mErr := job.Manifest()
	hasManifest := mErr == nil

	// The manifest goes to disk before the transaction opens, not inside it.
	// Writing it inside meant holding SQLite's write lock across an fsync,
	// which is the single widest window a competing writer can collide with.
	//
	// Before rather than after, so a failure is clean: no row is inserted at
	// all. The reverse ordering would leave a row whose manifest is missing.
	// A crash between the two leaves an orphan manifest instead, which Prune
	// already sweeps against the active job set.
	if hasManifest {
		manifestPath := filepath.Join(s.dir, "manifests", job.ID+".json.gz")
		if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
			return fmt.Errorf("sqlite store mkdir manifests: %w", err)
		}
		if err := writeGzJSON(manifestPath, m); err != nil {
			return fmt.Errorf("sqlite store write manifest %s: %w", job.ID, err)
		}
	}

	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		return s.addTx(ctx, tx, job, m, hasManifest)
	})
}

// addTx is Add's transactional half, split out so withWriteTx can run it
// again on a fresh transaction after contention. It must stay free of
// side effects outside the transaction — the manifest write is deliberately
// its caller's job.
func (s *SQLiteStore) addTx(ctx context.Context, tx *sql.Tx, job *Job, m *Manifest, hasManifest bool) error {
	var sortKey int
	row := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sort_key), -1) + 1 FROM jobs")
	if err := row.Scan(&sortKey); err != nil {
		return fmt.Errorf("sqlite store max sort_key: %w", err)
	}

	groupsJSON, _ := json.Marshal(job.Groups)
	metaJSON, _ := json.Marshal(job.Meta)

	var dlStartedUnix, dlFinishedUnix int64
	if p := job.Progress(); p != nil {
		dlStartedUnix = encodeJobStamp(p.DownloadStarted())
		dlFinishedUnix = encodeJobStamp(p.DownloadFinished())
	}

	const qJobs = `
INSERT INTO jobs
  (id, filename, name, password, url, category, priority, status, pp, script,
   time_added, md5, avg_age, groups, meta, warning, postproc, sort_key,
   download_started, download_finished, recovery_bytes, recovery_files, nzb_backup)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	postprocInt := 0
	if job.PostProc {
		postprocInt = 1
	}

	_, err := tx.ExecContext(ctx, qJobs,
		job.ID, job.Filename, job.Name, job.Password, job.URL, job.Category,
		int(job.Priority), string(job.Status), job.PP, job.Script,
		job.Added.Unix(), job.MD5, job.AvgAge.Unix(), string(groupsJSON), string(metaJSON),
		job.Warning, postprocInt, sortKey, dlStartedUnix, dlFinishedUnix,
		// From the promoted scalars rather than job.Manifest(): they are set
		// at Add and stay correct while the manifest is evicted, so this
		// writes the right values even for a job persisted while non-resident.
		job.RecoveryBytes(), job.RecoveryFiles(),
		job.NZBBackup,
	)
	if err != nil {
		return fmt.Errorf("sqlite store insert job %s: %w", job.ID, err)
	}

	if hasManifest {
		return insertJobFilesTx(ctx, tx, job, m)
	}
	return nil
}

// insertJobFilesTx writes one job_files row per file in m, taking every
// per-file value from job's progress. It assumes no rows exist for job.ID yet:
// file_index is the primary key, so callers replacing an existing set must
// delete it first.
//
// Shared by addTx and nothing else now — it was written to be shared with
// ReplaceManifest, which no longer exists because the file set is immutable
// after Add. Kept as a function because the row shape is worth naming in one
// place: a column a writer forgets is a column that silently reverts, which
// is what #287 was.
func insertJobFilesTx(ctx context.Context, tx *sql.Tx, job *Job, m *Manifest) error {
	const qFiles = `
INSERT INTO job_files
  (job_id, file_index, subject, date, bytes, is_par2_recovery, complete, fetch_policy, filename, assembled_crc32, article_count, failed_bytes, bytes_downloaded)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	p := job.Progress()
	for i := range m.NumFiles() {
		isPar2 := 0
		if m.FileIsPar2Recovery(i) {
			isPar2 = 1
		}
		complete := 0
		if p.FileComplete(i) {
			complete = 1
		}
		// fetch_policy comes from the job, not a literal zero. Hard-coding
		// it discarded on-demand par2's whole effect (#287) back when this
		// column was `deferred`: NewJob holds each recovery volume in
		// JobProgress, the INSERT wrote 0, and the first promotion read that
		// back over the live value.
		fetch := int(p.FileFetchPolicy(i))
		lo, hi := m.FileRange(i)
		_, err := tx.ExecContext(ctx, qFiles,
			job.ID, i, m.FileSubject(i), m.FileDate(i).Unix(), m.FileBytes(i), isPar2,
			complete, fetch,
			p.FileFilename(i), p.FileAssembledCRC32(i),
			hi-lo,
			// Caches of a sum over the file's article resolution, which now
			// lives in durable_runs and failed_articles rather than in a
			// column on this row. Still written by the single statement that
			// writes the rest of the row, and still superseded wholesale by
			// JobProgress.recompute on promotion — which is what
			// distinguishes them from the derived columns removed in #306.
			p.FileFailedBytes(i), p.FileBytesDownloaded(i),
		)
		if err != nil {
			return fmt.Errorf("sqlite store insert job_file %s/%d: %w", job.ID, i, err)
		}
	}
	return nil
}

// Get retrieves an active job by ID, reconstructing it from SQLite and its manifest.
func (s *SQLiteStore) Get(ctx context.Context, id string) (*Job, error) {
	const qJob = `
SELECT id, filename, name, COALESCE(password, ''), COALESCE(url, ''), COALESCE(category, ''), priority, status, pp, COALESCE(script, ''),
       time_added, md5, avg_age, COALESCE(groups, ''), COALESCE(meta, ''), COALESCE(warning, ''), postproc,
       download_started, download_finished, recovery_bytes, recovery_files,
       COALESCE(nzb_backup, '')
FROM jobs WHERE id = ?`

	var job Job
	var groupsStr, metaStr, statusStr string
	var priorityInt, ppInt, postprocInt, recoveryFiles int
	var addedUnix, avgAgeUnix, dlStartedUnix, dlFinishedUnix, recoveryBytes int64

	err := s.db.QueryRowContext(ctx, qJob, id).Scan(
		&job.ID, &job.Filename, &job.Name, &job.Password, &job.URL, &job.Category,
		&priorityInt, &statusStr, &ppInt, &job.Script, &addedUnix, &job.MD5, &avgAgeUnix,
		&groupsStr, &metaStr, &job.Warning, &postprocInt,
		&dlStartedUnix, &dlFinishedUnix, &recoveryBytes, &recoveryFiles,
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
		s.removeCorrupt(ctx, id)
		return nil, fmt.Errorf("sqlite store count files for %s: %w", id, err)
	}

	manifestPath := filepath.Join(s.dir, "manifests", id+".json.gz")
	if fileCount > 0 {
		if _, err := os.Stat(manifestPath); err != nil {
			s.removeCorrupt(ctx, id)
			return nil, fmt.Errorf("sqlite store manifest missing for %s: %w", id, err)
		}
	}

	if isResidentStatus(job.Status) {
		if _, err := os.Stat(manifestPath); err == nil {
			var manifest Manifest
			if err := readGzJSON(manifestPath, &manifest); err != nil {
				_ = os.Rename(manifestPath, manifestPath+".corrupt")
				s.removeCorrupt(ctx, id)
				return nil, fmt.Errorf("sqlite store read manifest %s: %w", id, err)
			}
			job.manifest = &manifest
			job.setScalarsFromManifest(&manifest)
			job.progress = newJobProgress(&manifest)
			job.progress.restoreDownloadStamps(
				decodeJobStamp(dlStartedUnix), decodeJobStamp(dlFinishedUnix))
			_ = s.RestoreJobProgress(ctx, &job)
		}
	}

	// job.manifest is nil here for a non-resident job (or a resident one
	// whose manifest file is missing, though that case already errored
	// above when fileCount > 0). Without a resident manifest,
	// TotalBytes/NumFiles/NumArticles would otherwise read as zero — a
	// gap left open by Task 3 — so reconstruct the three of the five
	// scalars that job_files can answer on its own. recoveryBytes/recoveryFiles
	// stay out of it; see setAggregateScalarsFromFiles for why a soft-failing
	// aggregate is the wrong home for them even though it could compute them.
	//
	// A query failure here is logged rather than failing Get outright: the
	// file-count query above fails closed because its result gates whether
	// the caller gets a job back at all (admission), but this query only
	// feeds three reporting scalars for a job that is otherwise valid and
	// already fully loaded. Returning the job with those three at zero (its
	// pre-Task-4 behavior) is preferable to losing the job entirely over a
	// transient SQLite error on an already-slow, non-resident read path —
	// but the error must not vanish silently, so it goes to the log.
	// The recovery figures come from the job row, not from job_files, and are
	// set independently of the aggregate query below so a failure there
	// cannot take them down with the other three. That separation is the
	// point: zero recovery bytes is a definite claim of no repair capacity,
	// not an absent reading. A resident job already has the authoritative
	// values from setScalarsFromManifest above.
	if job.manifest == nil {
		job.setRecoveryScalarsFromStore(recoveryBytes, recoveryFiles)
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
	// No byte or cursor figure is READ here. The recompute at the end of this
	// function derives Bytes, BytesDownloaded and FailedBytes from the manifest
	// and the article bitmaps, which is why this query selects neither
	// job_files.failed_bytes nor job_files.bytes_downloaded even though both
	// columns exist — they are for the NON-resident path (ArticleCountsByJob),
	// where there is no manifest to recompute from;
	// The write cursor and high-water mark are gone entirely: the completion
	// truncate derives its bound from the file's durable runs rather than from
	// a seed, and the coalescing cursor is local to the assembler's cache.
	// The job's article resolution, derived once for the whole job from
	// durable_runs and failed_articles. It replaces the per-file
	// job_files.articles_done blob this loop used to unpack, and it is read
	// before the row scan so a failure here surfaces before any bit is
	// installed.
	done, failed, err := s.resolutionForJob(ctx, job.ID, job.manifest.NumArticles())
	if err != nil {
		return err
	}
	const qFiles = `
SELECT file_index, complete, fetch_policy, assembled_crc32,
       COALESCE(filename, '')
FROM job_files WHERE job_id = ? ORDER BY file_index ASC`
	rows, err := s.db.QueryContext(ctx, qFiles, job.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var idx, complete, fetch int
		var crc32Val uint32
		var filename string
		if err := rows.Scan(&idx, &complete, &fetch, &crc32Val, &filename); err != nil {
			return fmt.Errorf("sqlite store scan job_file for %s: %w", job.ID, err)
		}
		if idx >= 0 && idx < len(job.progress.files) {
			fp := &job.progress.files[idx]
			fp.AssembledCRC32 = crc32Val
			// The resolved on-disk name, which this path used to drop while
			// still persisting it — RestoreRetryProgress's "unlike
			// RestoreJobProgress" note described a real asymmetry, and it was
			// not a deliberate one.
			//
			// Two things depend on it and both were silently broken across a
			// restart. pipeline.registerFile treats an empty name as "first
			// time resolving this file" and calls GetUniqueFilename, which
			// only returns a path that does not already exist — so a restart
			// resumed into <name>.1.<ext> (the counter goes before the
			// extension) and orphaned every byte the previous run
			// had written, which is the opposite of what registerFile's own
			// comment about "preventing duplicate renaming across daemon
			// restarts" claims. And the startup resume sweep locates a job's
			// file by this name, so with it empty there was nothing to resume
			// against and L3 could not hold.
			//
			// Restoring it is not a second writer of anything: job_files.filename
			// is written by SetFileFilename and read here, and a file that was
			// never registered still restores as empty.
			fp.Filename = filename
			// The derived article resolution is the only source of
			// per-article state THIS FUNCTION reads; Complete is just a flag
			// alongside it.
			//
			// It is not the last word in the process, and must not be read as
			// one. The startup resume sweep (internal/app/resume_startup.go)
			// stats each file against the runs recorded for it and installs
			// its answer through Queue.ReplaceFromRuns, which OVERWRITES the
			// per-article bits restored here for every file it resumed —
			// including clearing one. What this function restores is what the
			// record says; what the sweep installs is what the record says
			// AFTER a file shorter than its runs claim has had them discarded.
			//
			// The precedence used to run the other way, and that was #362: the
			// sweep seeded through an additive entry point that never cleared
			// a bit, so an article this restore marked done stayed done even
			// after the file was shown to be missing, and the job completed a
			// file with a zero-filled hole in it.
			//
			// The sweep's authority is bounded to the files it actually read.
			// A file it never resumed — a job that is not downloading or not
			// resident, a file whose name was never resolved, a file a storage
			// fault stopped it reaching — keeps exactly what this function
			// restored, because an omission is silence rather than a finding of
			// absence. The phase bound is the load-bearing one: past
			// downloading, par2 and the move rewrite these files, so the
			// download-time record no longer describes them and re-deriving
			// over them would clear correct state. A permanently failed
			// article keeps its bits for the same reason: its bytes were never
			// on disk, so their absence is the outcome recorded below and not
			// new evidence.
			//
			// Complete used to short-circuit the restore and mark every
			// article of the file done. That is wrong because Complete means
			// "the assembler is finished with this file", not "every article
			// arrived" — internal/app/pipeline.go hands a permanently failed
			// article to the assembler, which closes the file with a gap in
			// it. So a restart rewrote failed articles as successful ones,
			// and since JobProgress.recompute derives failedBytes from those
			// bits the job came back reporting full health while a retry
			// found nothing to refetch (#300).
			//
			// One coupling did go with the old branch. Force-marking every
			// article done meant recompute always derived Pending == 0 for a
			// complete file; recompute never consults Complete, so that no
			// longer holds. It is cosmetic — ForEachUnfinishedArticle skips
			// on Complete before reading Pending, and IsComplete drives
			// post-processing off the flags — but do not assume the
			// implication still exists.
			lo, hi := job.manifest.FileRange(idx)
			applyResolution(job, done, failed, lo, hi)
			if complete != 0 {
				fp.Complete = true
			}
			fp.Fetch = FetchPolicy(fetch) //nolint:gosec // G115: fetch_policy is 0-2, fits in uint8
		}
	}
	// Sole source of per-file byte state on this path: Bytes from the
	// manifest, BytesDownloaded and FailedBytes from the article bitmaps
	// applyResolution just installed. Removing this call does not fall back
	// on the columns — they are not read above — it leaves every figure at
	// zero, and RemainingBytes would report a fully downloaded job.
	job.progress.recompute(job.manifest)
	return rows.Err()
}

// ArticleCountsByJob returns every job's per-file FileMeta — article count,
// byte size, failed bytes, downloaded bytes, and complete/fetch
// state — in a single grouped query, indexed by file_index within each job.
// Used to size JobProgress at restart without loading each job's manifest individually,
// and to give newJobProgressSized everything it needs to reconstruct a
// non-resident job's RemainingBytes, so a large queued backlog costs one
// round trip instead of one query per job.
//
// Entries are placed by file_index rather than by scan order, so a job
// whose job_files rows have non-contiguous indices (e.g. after a partial
// delete) still gets each entry attributed to the right file instead of
// shifted by position.
func (s *SQLiteStore) ArticleCountsByJob(ctx context.Context) (map[string][]FileMeta, error) {
	// Every column comes from job_files, including both byte figures. The
	// downloaded figure used to be read from the durability record instead,
	// which was the wrong quantity: the record's lengths are DECODED payload
	// bytes, while the remaining figure this feeds subtracts from jf.bytes, an
	// ENCODED NZB total. The skew is the encoding overhead, a few percent, and
	// it made a non-resident job report more bytes outstanding than the same
	// job resident — the exact divergence
	// TestRemainingBytes_IdenticalResidentAndNonResident forbids.
	// jf.bytes_downloaded sums the same articles in the same unit as jf.bytes.
	//
	// One grouped query for the whole queue, which is what keeps restart at
	// B3's O(incomplete files) bound. Reading per job instead would be N
	// queries at startup.
	// Per-article resolution is no longer a column here; it is derived from
	// durable_runs and failed_articles by the two whole-table sweeps below.
	// Without it a non-resident job's every article reads as Pending.
	const q = `
SELECT jf.job_id, jf.file_index, jf.article_count, jf.bytes, jf.complete,
       jf.fetch_policy, jf.subject, jf.failed_bytes, jf.bytes_downloaded
  FROM job_files jf
 ORDER BY jf.job_id ASC, jf.file_index ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sqlite store article counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string][]FileMeta)
	for rows.Next() {
		var jobID, subject string
		var idx, count int
		var fileBytes, failedBytes, bytesDownloaded int64
		var complete, fetch int
		if err := rows.Scan(&jobID, &idx, &count, &fileBytes, &complete, &fetch, &subject,
			&failedBytes, &bytesDownloaded); err != nil {
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
			grown := make([]FileMeta, idx+1)
			copy(grown, counts)
			counts = grown
		}
		counts[idx] = FileMeta{
			ArticleCount: count,
			Bytes:        fileBytes,
			Complete:     complete != 0,
			Fetch:        FetchPolicy(fetch), //nolint:gosec // G115: fetch_policy is 0-2, fits in uint8
			// Classified from the stored subject rather than from
			// is_par2_recovery, which flags recovery volumes only. The par2
			// index is the case this exists for, and it is not a volume.
			IsPar2:          isPar2File(subject),
			BytesDownloaded: bytesDownloaded,
			FailedBytes:     failedBytes,
		}
		result[jobID] = counts
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite store article counts: %w", err)
	}
	if err := s.fillResolution(ctx, result); err != nil {
		return nil, err
	}
	return result, nil
}

// fillResolution derives every job's per-file Done/Failed flags from
// durable_runs and failed_articles, in TWO whole-table sweeps rather than a
// pair of queries per job.
//
// # The boot cost, and why this shape
//
// The rule this replaces was a blob on job_files, so it arrived free with the
// row. Runs and failed articles live in their own tables, and the question
// "what does every non-resident job still have to fetch" has to be answered
// for the whole queue at startup — B3 bounds that at O(incomplete files), and
// a per-job pair of queries would make it O(jobs) round trips instead.
//
// Two ungrouped SELECTs over the two tables, ordered by job_id, is the cheapest
// shape available and is strictly cheaper than what it replaces: a run covers
// a whole span of articles in one row, where the old blob carried two bits per
// article. A job that downloaded in order has ONE run row. The failed table is
// bounded by the defect count, not the article count.
//
// Neither sweep is bounded to the jobs in `result`, and that is deliberate
// rather than sloppy: a WHERE ... IN over every queued job ID is a bigger
// statement than the scan it saves.
//
// The scoping is a SIZE question, not a correctness one, and it is worth being
// precise about which. `result` is built from a whole-table job_files scan
// before either sweep runs, and every scanned row is filtered through a
// membership test against it below — so a row belonging to a departed job is
// discarded in Go and cannot reach any job's FileMeta. The answer is the same
// whatever the tables hold. What the scoping buys is only the cost of reading
// rows nobody wants, and SQLiteStore.Prune is what keeps that bounded by
// removing rows whose job is in neither the queue nor history-as-FAILED. If
// Prune ever weakened, these sweeps would get slower at boot; they would not
// get wrong.
//
// # Deriving the file-local index without a manifest
//
// FileMeta.Done and .Failed are indexed FILE-LOCALLY, while a run and a failed
// row both name articles GLOBALLY. The conversion needs each file's first
// global index, which the manifest derives by accumulating article counts in
// file order (Manifest.fileArticleOffsets) — and `result` holds exactly those
// counts, placed by file_index. So the running sum reproduces it, which is the
// same derivation newJobProgressSized already makes for the same reason.
func (s *SQLiteStore) fillResolution(ctx context.Context, result map[string][]FileMeta) error {
	if len(result) == 0 {
		return nil
	}
	runsByJob := make(map[string][]artRange)
	if err := s.scanAll(ctx,
		`SELECT job_id, first_art_idx, last_art_idx FROM durable_runs ORDER BY job_id`,
		"durable runs",
		func(rows *sql.Rows) error {
			var jobID string
			var r artRange
			if err := rows.Scan(&jobID, &r.First, &r.Last); err != nil {
				return err
			}
			if _, ok := result[jobID]; ok {
				runsByJob[jobID] = append(runsByJob[jobID], r)
			}
			return nil
		}); err != nil {
		return err
	}
	failedByJob := make(map[string][]int32)
	if err := s.scanAll(ctx,
		`SELECT job_id, art_idx FROM failed_articles ORDER BY job_id`,
		"failed articles",
		func(rows *sql.Rows) error {
			var jobID string
			var idx int32
			if err := rows.Scan(&jobID, &idx); err != nil {
				return err
			}
			if _, ok := result[jobID]; ok {
				failedByJob[jobID] = append(failedByJob[jobID], idx)
			}
			return nil
		}); err != nil {
		return err
	}

	for jobID, files := range result {
		total := 0
		for _, f := range files {
			total += f.ArticleCount
		}
		if total == 0 {
			continue
		}
		done, failed := resolveArticles(runsByJob[jobID], failedByJob[jobID], total)
		base := 0
		for i := range files {
			n := files[i].ArticleCount
			files[i].Done = done[base : base+n]
			files[i].Failed = failed[base : base+n]
			base += n
		}
	}
	return nil
}

// scanAll runs one whole-table query and hands each row to fn.
//
// Shared by fillResolution's two sweeps, which differ only in their statement
// and their row shape. The `what` string names the table in both error paths,
// so a failure says which sweep failed rather than only that one did.
func (s *SQLiteStore) scanAll(ctx context.Context, query, what string, fn func(*sql.Rows) error) error {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("sqlite store %s sweep: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return fmt.Errorf("sqlite store scan %s: %w", what, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite store %s sweep: %w", what, err)
	}
	return nil
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
	if p := job.Progress(); p != nil {
		dlStartedUnix = encodeJobStamp(p.DownloadStarted())
		dlFinishedUnix = encodeJobStamp(p.DownloadFinished())
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
		// failed_bytes and bytes_downloaded are written here for the same
		// reason they are in insertJobFilesTx: each caches a sum over the
		// file's article resolution, and both are superseded wholesale by
		// JobProgress.recompute on promotion.
		const qF = `UPDATE job_files SET complete = ?, fetch_policy = ?, filename = ?, assembled_crc32 = ?, article_count = ?, failed_bytes = ?, bytes_downloaded = ? WHERE job_id = ? AND file_index = ?`
		for i := range m.NumFiles() {
			complete := 0
			if job.Progress().FileComplete(i) {
				complete = 1
			}
			fetch := int(job.Progress().FileFetchPolicy(i))
			lo, hi := m.FileRange(i)
			if _, err := execer.ExecContext(ctx, qF,
				complete, fetch, job.Progress().FileFilename(i), job.Progress().FileAssembledCRC32(i), hi-lo,
				job.Progress().FileFailedBytes(i), job.Progress().FileBytesDownloaded(i),
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

// removeCorrupt removes a job whose persisted state this store has just found
// unusable — a missing or unreadable manifest, or a job_files count that could
// not be read — and takes its durability rows with it.
//
// Ordinary removal deliberately does NOT do that. Queue.Remove runs on the
// queue-to-history transition as well, where a FAILED job's durable runs are
// retained on purpose: a retry reuses the job ID and the same partial file,
// and those runs are what bound FinalizeFile's truncate to the whole file
// rather than to the few articles the retry re-fetches. Sweeping them in
// Remove would destroy that.
//
// This path is different in the way that matters. It is reached only when the
// job's own manifest is gone or unreadable, and every run and every failed
// article is keyed by a global article index that only a manifest can
// interpret. There is nothing left to read them against, and no queue row and
// no history entry will arrive to collect them, so retaining them is not
// caution — it is a leak of rows nothing can use.
func (s *SQLiteStore) removeCorrupt(ctx context.Context, id string) {
	_ = s.Remove(ctx, id)
	for _, q := range []string{
		"DELETE FROM durable_runs WHERE job_id = ?",
		"DELETE FROM failed_articles WHERE job_id = ?",
	} {
		if _, err := s.db.ExecContext(ctx, q, id); err != nil {
			// Not fatal: the caller is already returning an error about the
			// corruption it found, and leaving these rows is a leak rather
			// than a correctness failure. Not silent either (A2).
			s.log.Warn("could not drop the durability rows of a job removed for corrupt state",
				"job", id, "err", err)
		}
	}
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
  (job_id, file_index, complete, fetch_policy, filename, assembled_crc32, article_count)
SELECT job_id, file_index, complete, fetch_policy, filename, assembled_crc32, article_count
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
// retry rebuilds the manifest by re-parsing the NZB, and the durable runs and
// failed-article rows the retry reads are indexed against the manifest that
// produced them. Applying them to a manifest with a different shape would mark
// the wrong articles done and silently skip real downloads, so the counts must
// match before the overlay is used.
//
// The per-article state itself is no longer carried here. It used to be an
// ArticlesDone bitmap copied from job_files at MoveToHistory; it now stays in
// durable_runs and failed_articles, which are retained for a FAILED job by the
// same rule and dropped by the same history.Delete. This row carries what
// those two tables cannot: the file's resolved on-disk name, its complete
// flag, its assembled CRC, and the alignment count.
type RetainedFile struct {
	FileIndex      int
	Complete       bool
	Fetch          FetchPolicy
	Filename       string
	AssembledCRC32 uint32
	ArticleCount   int
}

// HistoryFileProgress returns the per-file progress retained for a failed
// history job, ordered by file index. An empty slice means nothing was
// retained — the job succeeded, its entry predates retention, or its rows
// were already deleted — and is not an error.
func (s *SQLiteStore) HistoryFileProgress(ctx context.Context, jobID string) ([]RetainedFile, error) {
	const q = `
SELECT file_index, complete, fetch_policy,
       COALESCE(filename, ''), COALESCE(assembled_crc32, 0), article_count
FROM history_job_files WHERE job_id = ? ORDER BY file_index ASC`
	rows, err := s.db.QueryContext(ctx, q, jobID)
	if err != nil {
		return nil, fmt.Errorf("sqlite store history file progress %s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []RetainedFile
	for rows.Next() {
		var f RetainedFile
		var complete, fetch int
		if err := rows.Scan(&f.FileIndex, &complete, &fetch,
			&f.Filename, &f.AssembledCRC32, &f.ArticleCount); err != nil {
			return nil, fmt.Errorf("sqlite store scan history_job_file %s: %w", jobID, err)
		}
		f.Complete = complete != 0
		f.Fetch = FetchPolicy(fetch) //nolint:gosec // G115: fetch_policy is 0-2, fits in uint8
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite store history file progress rows %s: %w", jobID, err)
	}
	return out, nil
}

// RestoreRetryProgress overlays a failed job's retained per-file progress
// onto a job rebuilt by re-parsing its NZB, so the retry refetches only the
// articles that did not succeed. It reports whether the overlay was applied.
//
// Not applied is an ordinary outcome, not an error: nothing was retained
// (the job completed, or its entry predates retention), or the rebuilt
// manifest does not match the shape the retained rows were written against.
// Both mean "download this job from scratch", which is correct if wasteful.
//
// The alignment check exists because a durable run and a failed-article row
// both name articles by an index into the manifest that produced them.
// Re-parsing the same NZB bytes is deterministic and yields the same shape, so
// a mismatch means the recorded backup is not the NZB this job was built from.
// Applying the rows anyway would mark the wrong articles done — and a done
// article is never
// requested, so the job would finish "successfully" with missing data. The
// cost of refusing is bounded and visible; the cost of applying is silent
// corruption.
func (s *SQLiteStore) RestoreRetryProgress(ctx context.Context, job *Job) (bool, error) {
	if job == nil || job.progress == nil || job.manifest == nil {
		return false, nil
	}
	retained, err := s.HistoryFileProgress(ctx, job.ID)
	if err != nil {
		return false, err
	}
	if len(retained) == 0 {
		return false, nil
	}
	if !retainedMatchesManifest(retained, job.manifest) {
		s.log.Warn("retained progress does not match the re-parsed NZB, retrying from scratch",
			"job_id", job.ID, "retained_files", len(retained), "manifest_files", job.manifest.NumFiles())
		return false, nil
	}
	// Read only after the alignment check has passed, because that check is
	// what makes the article indices in these rows interpretable at all.
	done, failed, err := s.resolutionForJob(ctx, job.ID, job.manifest.NumArticles())
	if err != nil {
		return false, err
	}

	for _, f := range retained {
		fp := &job.progress.files[f.FileIndex]
		fp.AssembledCRC32 = f.AssembledCRC32
		// The resolved on-disk filename is carried over, the same as
		// RestoreJobProgress now does — that used to be an asymmetry this
		// comment pointed at, and it was a defect there rather than a design
		// choice here. A retry can go straight back into post-processing
		// without a download pass (every article already resolved), and
		// postproc's file list and QuickCheck read this name.
		fp.Filename = f.Filename
		// Same precedence as RestoreJobProgress, and for a sharper reason.
		// ResetForRetry clears exactly the articles whose failed bit is set,
		// so a complete file restored as all-done-none-failed leaves it with
		// nothing to reset: the rebuilt job looks fully downloaded and goes
		// straight back to post-processing, where it fails identically.
		// Retry would appear to run and change nothing (#300).
		lo, hi := job.manifest.FileRange(f.FileIndex)
		applyResolution(job, done, failed, lo, hi)
		if f.Complete {
			fp.Complete = true
		}
		// A retry re-derives the par2 verdict rather than inheriting it, for
		// the same reason ResetForRetry gives: the articles this retry
		// re-fetches can change the contents the oracle already ruled on, so
		// a discarded volume (FetchNever) and a still-held one (FetchIfNeeded)
		// both land back on FetchIfNeeded here. That symmetry with
		// ResetForRetry's downgrade covers only this held/discarded pair —
		// it does not extend to FetchAlways, where the two routes genuinely
		// diverge (see below), and this overlay makes no attempt to close
		// that gap.
		//
		// A retained FetchAlways is the one value deliberately left alone.
		// fp already carries whatever NewJob just decided while rebuilding the
		// job from the re-parsed NZB — FetchIfNeeded on every recovery volume
		// when OnDemandPar2 is on (see NewJob) — and that fresh decision is
		// what must win, not the row: an undeferRecovery release (a
		// permanent article failure, or a completed download that needed
		// repair) sets FetchAlways and persists it, and a subsequent
		// MoveToHistory retains that value. Assigning fp.Fetch = f.Fetch
		// unconditionally (an unqualified else branch) would let that stale
		// FetchAlways silently overwrite the fresh hold NewJob just set,
		// re-downloading a recovery volume the current rebuild has no reason
		// to think it needs — see
		// TestRestoreRetryProgress_DoesNotClearAHoldTheRebuiltJobJustSet.
		//
		// This is where the two retry routes actually disagree. On the live
		// route (Queue.Retry -> ResetForRetry, job.go), a volume that
		// undeferRecovery released to FetchAlways because damage was
		// found is not FetchNever, so ResetForRetry's downgrade skips it and
		// it stays FetchAlways — re-downloaded. On this history route, that
		// same retained FetchAlways is skipped by the condition below, so fp
		// keeps NewJob's fresh FetchIfNeeded — held pending a fresh ruling.
		// Same prior state, opposite outcome, decided only by whether the
		// job was still queue-resident when the user retried it.
		// ResetForRetry cannot special-case that release the way this
		// overlay does: JobFile.Deferred (the record of on-demand opt-in) is
		// not carried onto Manifest, which exposes only FileIsPar2Recovery,
		// so a live job's ResetForRetry has no way to tell an opted-in job's
		// damage-released volume from an ordinary FetchAlways on a content
		// file. Closing that gap needs state this task does not have.
		if f.Fetch == FetchIfNeeded || f.Fetch == FetchNever {
			fp.Fetch = FetchIfNeeded
		}
	}
	job.progress.recompute(job.manifest)
	return true, nil
}

// retainedMatchesManifest reports whether retained describes exactly the
// files m has, in the same order and with the same article counts. See
// RestoreRetryProgress for why anything less is refused rather than
// best-effort merged.
func retainedMatchesManifest(retained []RetainedFile, m *Manifest) bool {
	if len(retained) != m.NumFiles() {
		return false
	}
	for i, f := range retained {
		if f.FileIndex != i {
			return false
		}
		lo, hi := m.FileRange(i)
		if f.ArticleCount != hi-lo {
			return false
		}
	}
	return true
}

// DeleteJobArtifacts removes the on-disk manifest for job id. See the Store
// interface doc comment for the ordering requirement this depends on and why
// deletion is best-effort.
func (s *SQLiteStore) DeleteJobArtifacts(_ context.Context, id string) error {
	if err := os.Remove(filepath.Join(s.dir, "manifests", id+".json.gz")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove manifest: %w", err)
	}
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
	// Reads the whole ordering, then rewrites it: the same read-then-write
	// upgrade as Add, and so the same need to restart on contention.
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		return s.shiftSortKeyTx(ctx, tx, id, newIndex)
	})
}

func (s *SQLiteStore) shiftSortKeyTx(ctx context.Context, tx *sql.Tx, id string, newIndex int) error {
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
	return nil
}

// Prune removes orphaned filesystem files not present in SQLite, and the
// durability rows of jobs that are in neither the queue nor history-as-FAILED
// (see pruneDurabilityRows).
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
	cleanDir("jobs")
	_ = os.Remove(filepath.Join(s.dir, "queue.json.gz"))
	return s.pruneDurabilityRows(ctx)
}

// pruneDurabilityRows is the backstop for the durable runs and failed-article
// rows a departed job left behind. Neither table has a foreign key to jobs, so
// nothing collects them implicitly.
//
// The ordinary paths already cover this — Application.deleteJobDurability on
// the queue-to-history transition, removeCorrupt on the self-healing removals
// in Get, Application.dropJobAlreadyInHistory on the startup reconcile, and the
// history entry's own deletion. What is left is the crash window between a job
// leaving jobs and its rows being deleted, after which nothing looks for them
// again for the life of the installation.
//
// # Why the obvious predicate is wrong
//
// "job_id not in jobs" would sweep a job that moved to history as FAILED, and
// those rows are kept DELIBERATELY: a retry uses them to bound
// Barrier.FinalizeFile's truncate to the whole partial file rather than to the
// few articles the retry re-fetches. dropJobAlreadyInHistory encodes the same
// rule by returning early on StatusFailed. A sweep that ignores it destroys
// exactly the ground the retry path exists to preserve, and does so silently.
//
// # Why NOT EXISTS rather than NOT IN
//
// SQLite permits NULL in a TEXT PRIMARY KEY, so jobs.id and history.nzo_id can
// both hold one. A single NULL anywhere in a NOT IN subquery makes the
// predicate NULL for EVERY row, and the delete then matches nothing — a sweep
// that silently stops sweeping, with no error and no way to notice from
// outside. NOT EXISTS compares row by row and is unaffected.
//
// The failure direction of that trap is retention rather than loss, which is
// why it is worth naming: it would not have shown up as a bug, only as this
// function quietly never working.
//
// The two statements are written out rather than generated over a table-name
// list. The predicate has to name its own table three times, so a templated
// form needs SQL string concatenation for a value that is a compile-time
// constant either way -- all cost, no reuse.
func (s *SQLiteStore) pruneDurabilityRows(ctx context.Context) error {
	const runs = `
		DELETE FROM durable_runs
		 WHERE NOT EXISTS (SELECT 1 FROM jobs j WHERE j.id = durable_runs.job_id)
		   AND NOT EXISTS (SELECT 1 FROM history h
		                    WHERE h.nzo_id = durable_runs.job_id AND h.status = ?)`
	const failed = `
		DELETE FROM failed_articles
		 WHERE NOT EXISTS (SELECT 1 FROM jobs j WHERE j.id = failed_articles.job_id)
		   AND NOT EXISTS (SELECT 1 FROM history h
		                    WHERE h.nzo_id = failed_articles.job_id AND h.status = ?)`
	// Errors are RETURNED, unlike the cleanDir calls above, which ignore
	// theirs. A missing file is one orphan; a failing DELETE is the sweep not
	// running at all, and Queue.persist propagates it so the next save retries
	// rather than reporting a prune that did not happen.
	for _, q := range []string{runs, failed} {
		if _, err := s.db.ExecContext(ctx, q, string(constants.StatusFailed)); err != nil {
			return fmt.Errorf("sqlite store prune durability rows: %w", err)
		}
	}
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
