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

	_, err := tx.ExecContext(ctx, qJobs,
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

	if hasManifest {
		return insertJobFilesTx(ctx, tx, job, m)
	}
	return nil
}

// insertJobFilesTx writes one job_files row per file in m, taking every
// per-file value from job's progress. It assumes no rows exist for job.ID yet:
// job_files carries UNIQUE(job_id, file_index), so callers replacing an
// existing set must delete it first. (The primary key is the surrogate
// `id INTEGER PRIMARY KEY AUTOINCREMENT`; it is the unique index, not the
// key, that rejects the second insert.)
//
// Shared by addTx and ReplaceManifest rather than duplicated. Those are the
// only two writers of this table's full row shape, and they must agree: a
// column one of them forgets is a column that silently reverts whenever the
// other runs. #287 was exactly that failure with `deferred` hard-coded to
// zero in the sole writer of the day.
func insertJobFilesTx(ctx context.Context, tx *sql.Tx, job *Job, m *Manifest) error {
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
		_, err := tx.ExecContext(ctx, qFiles,
			job.ID, i, m.FileSubject(i), m.FileDate(i).Unix(), m.FileBytes(i), isPar2,
			complete, deferred,
			p.FileWriteCursor(i), p.FileBytesDownloaded(i), p.FileFilename(i), p.FileAssembledCRC32(i),
			artDoneStr, hi-lo,
		)
		if err != nil {
			return fmt.Errorf("sqlite store insert job_file %s/%d: %w", job.ID, i, err)
		}
	}
	return nil
}

// ReplaceManifest rewrites job's stored manifest and replaces its job_files
// rows to match. Two callers, with different synchronization — see
// Store.ReplaceManifest's doc comment: DiscardDeferredPar2 under q.mu, and
// Queue.reconcileJobFiles outside it to retry a discard that could not
// persist.
//
// Every row is rewritten rather than the discarded ones deleted. Dropping a
// file renumbers every file_index after it, so a targeted DELETE would leave
// the survivors' stored progress attached to the wrong articles — a quieter
// bug than the one this fixes.
//
// The manifest blob is written before the transaction opens, for the reason
// Add gives: writing it inside means holding SQLite's write lock across an
// fsync, the widest window a competing writer can collide with. Neither
// ordering is atomic across the two media, so a crash between them leaves the
// blob and the rows describing different shapes, bounded by one fsync plus one
// small transaction where the state this replaces was a permanent
// disagreement on every discard.
//
// That window is closed on the way back in, not here. The staleness guard in
// hydrateSnapshot/hydrateJobLocked does not close it: those compare a live
// JobProgress against a manifest read from disk, and on the restart path the
// progress was built from that same manifest, so they agree by construction.
// The disagreement is between the two stored artifacts, and only the row
// indices carry it — RestoreJobProgress range-checks them and reports
// ErrManifestStale, and SQLiteStore.Get finishes this interrupted write via
// reconcileJobFiles (#310).
func (s *SQLiteStore) ReplaceManifest(ctx context.Context, job *Job) error {
	m, err := job.Manifest()
	if err != nil {
		return fmt.Errorf("sqlite store replace manifest %s: %w", job.ID, err)
	}
	manifestPath := filepath.Join(s.dir, "manifests", job.ID+".json.gz")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
		return fmt.Errorf("sqlite store mkdir manifests: %w", err)
	}
	if err := writeGzJSON(manifestPath, m); err != nil {
		return fmt.Errorf("sqlite store write manifest %s: %w", job.ID, err)
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM job_files WHERE job_id = ?", job.ID); err != nil {
			return fmt.Errorf("sqlite store delete job_files %s: %w", job.ID, err)
		}
		return insertJobFilesTx(ctx, tx, job, m)
	})
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
			if err := s.RestoreJobProgress(ctx, &job); errors.Is(err, ErrManifestStale) {
				// A ReplaceManifest that did not finish. Complete it here
				// rather than degrade: this is the load path, it holds the
				// manifest the rows should have been written from, and it
				// runs once at startup where a write is affordable.
				//
				// Degrading instead would strand the job. ReplaceManifest is
				// only reached from DiscardDeferredPar2, which runs
				// mid-download, so the crash leaves the job at
				// StatusDownloading — and a Downloading job with no manifest
				// is invisible to ForEachUnfinishedArticle (which skips a nil
				// manifest) and to PromoteNext (which only considers
				// StatusQueued). Nothing normalises the status at startup, so
				// it would sit at "Downloading", dispatching nothing, for the
				// life of the process.
				s.log.Warn("stored manifest and job_files describe different file sets, finishing the interrupted rewrite",
					"job_id", id, "err", err)
				if rErr := s.reconcileJobFiles(ctx, &job); rErr != nil {
					// Reconciliation is the recovery; without it there is
					// none. Refuse residency rather than keep a manifest the
					// rows contradict, and record why — hydrateErr is what
					// separates "evicted, routine" from "could not be made
					// usable", which is the distinction a stranded job
					// otherwise hides.
					s.log.Error("could not reconcile stored manifest with job_files; loading the job without a manifest",
						"job_id", id, "err", rErr)
					// nil progress, not the one built from the manifest
					// being dropped: Queue.Load treats nil as "size this
					// from job_files", which is the artifact that survives
					// with its own shape intact. setHydrateFailure clears
					// the manifest itself.
					job.setHydrateFailure(nil, fmt.Errorf("queue: reconcile job %s: %w", id, rErr))
				}
			} else if err != nil {
				s.log.Error("could not restore job progress", "job_id", id, "err", err)
			}
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
	stored, err := s.storedFileRows(ctx, job.ID)
	if err != nil {
		return err
	}
	// Validate the whole set before applying any of it. Rows arrive in
	// file_index order, so applying as we scan would leave the first N files
	// overwritten and recompute never run when row N+1 turns out to be the
	// one that disagrees — progress half-belonging to two different file
	// sets, with its aggregates matching neither. Both callers that pass a
	// live JobProgress hand that same object back on failure as the
	// disk-backed value to fall back to, so a partial apply corrupts the
	// fallback rather than preserving it.
	for _, r := range stored {
		if r.idx < 0 || r.idx >= len(job.progress.files) {
			// The stored rows and the stored manifest describe different
			// file sets. Skipping the row silently is what let the rest of
			// them splice: dropping a file renumbers every file_index after
			// it, so the in-range rows are no longer describing the files
			// they land on (#310).
			//
			// This is the only place the disagreement is visible. The
			// in-process guard, JobProgress.describesSameJobAs, compares a
			// live progress against a manifest read from disk; on the paths
			// that reach here after a restart the progress was just built
			// from that same manifest, so it agrees by construction and the
			// guard cannot fire. Only the row indices carry the mismatch.
			return fmt.Errorf(
				"%w: stored job_files for job %s hold file_index %d but its stored manifest describes %d files",
				ErrManifestStale, job.ID, r.idx, len(job.progress.files))
		}
	}

	for _, r := range stored {
		fp := &job.progress.files[r.idx]
		fp.BytesDownloaded = r.bytesDownloaded
		fp.WriteCursor = r.writeCursor
		fp.AssembledCRC32 = r.assembledCRC32
		// articles_done is the only source of per-article state;
		// Complete is just a flag alongside it.
		//
		// Complete used to short-circuit the decode and mark every
		// article of the file done. That is wrong because Complete means
		// "the assembler is finished with this file", not "every article
		// arrived" — internal/app/pipeline.go hands a permanently failed
		// article to the assembler, which closes the file with a gap in
		// it. So a restart rewrote failed articles as successful ones,
		// and since JobProgress.recompute derives failedBytes from those
		// bits the job came back reporting full health while a retry
		// found nothing to refetch (#300).
		//
		// There is deliberately no fallback for an empty bitmap.
		// Defaulting to "all done" is the same override in miniature,
		// and in the safe direction an unknown article is one to fetch
		// again, not one to skip.
		//
		// Nor is one needed. encodeArticlesDone returns "" on four
		// branches — nil job, nil Progress, a Manifest() error, and a
		// zero-article file — but the first three cannot occur, because
		// neither write path calls it without residency: addTx encodes
		// under `if hasManifest`, and updateTx under
		// `job.Progress() != nil && mErr == nil`. Those guards, not
		// anything inside the encoder, are what establish the invariant.
		// The fourth is real and harmless: a file with no articles has
		// nothing for the removed loop to have marked.
		//
		// One coupling did go with the old branch. Force-marking every
		// article done meant recompute always derived Pending == 0 for a
		// complete file; recompute never consults Complete, so that no
		// longer holds. It is cosmetic — ForEachUnfinishedArticle skips
		// on Complete before reading Pending, and IsComplete drives
		// post-processing off the flags — but do not assume the
		// implication still exists.
		s.decodeArticlesDone(r.articlesDone, job, r.idx)
		if r.complete {
			fp.Complete = true
		}
		if r.deferred {
			fp.Deferred = true
		}
	}
	job.progress.recompute(job.manifest)
	return nil
}

// storedFileRow is one job_files row, in the shape the restore and reconcile
// paths both need. subject is the file's identity: it is the only column that
// survives a renumber, so it is what maps a pre-discard row onto the file it
// still describes.
type storedFileRow struct {
	idx             int
	subject         string
	complete        bool
	deferred        bool
	writeCursor     int64
	bytesDownloaded int64
	assembledCRC32  uint32
	filename        string
	articlesDone    string
}

// storedFileRows reads every job_files row for jobID, in file_index order.
func (s *SQLiteStore) storedFileRows(ctx context.Context, jobID string) ([]storedFileRow, error) {
	const q = `
SELECT file_index, subject, complete, deferred, write_cursor, bytes_downloaded, assembled_crc32, filename, COALESCE(articles_done, '')
FROM job_files WHERE job_id = ? ORDER BY file_index ASC`
	rows, err := s.db.QueryContext(ctx, q, jobID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []storedFileRow
	for rows.Next() {
		var r storedFileRow
		var complete, deferred int
		if err := rows.Scan(&r.idx, &r.subject, &complete, &deferred,
			&r.writeCursor, &r.bytesDownloaded, &r.assembledCRC32, &r.filename, &r.articlesDone); err != nil {
			return nil, fmt.Errorf("sqlite store scan job_file for %s: %w", jobID, err)
		}
		r.complete = complete != 0
		r.deferred = deferred != 0
		out = append(out, r)
	}
	return out, rows.Err()
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

	// The job_files half is keyed by file_index and takes every value from
	// the live manifest, so it is correct only while the stored rows describe
	// the same file set. When the job says they do not, writing anything by
	// index puts one file's progress on another file's row — worse than
	// writing nothing, because the rows are the durable copy and nothing
	// downstream can tell a spliced row from a real one (#310).
	//
	// Skipping leaves the rows at their last consistent shape. Queue.saveStore
	// retries the wholesale rewrite that reconciles them; until it succeeds,
	// this job's per-file counters simply do not advance on disk, which costs
	// re-downloading some articles after a crash and costs no correctness.
	if job.ManifestRowsStale() {
		s.log.Warn("skipping job_files update: the stored rows no longer describe this job's manifest",
			"job_id", job.ID)
	} else if m, mErr := job.Manifest(); job.Progress() != nil && mErr == nil {
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

// RestoreRetryProgress overlays a failed job's retained per-file progress
// onto a job rebuilt by re-parsing its NZB, so the retry refetches only the
// articles that did not succeed. It reports whether the overlay was applied.
//
// Not applied is an ordinary outcome, not an error: nothing was retained
// (the job completed, or its entry predates retention), or the rebuilt
// manifest does not match the shape the bitmap was written against. Both
// mean "download this job from scratch", which is correct if wasteful.
//
// The alignment check exists because articles_done is indexed positionally
// against the manifest that produced it. Re-parsing the same NZB bytes is
// deterministic and yields the same shape, so a mismatch means the recorded
// backup is not the NZB this job was built from. Applying the bitmap anyway
// would mark the wrong articles done — and a done article is never
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

	for _, f := range retained {
		fp := &job.progress.files[f.FileIndex]
		fp.BytesDownloaded = f.BytesDownloaded
		fp.WriteCursor = f.WriteCursor
		fp.AssembledCRC32 = f.AssembledCRC32
		// Unlike RestoreJobProgress, the resolved on-disk filename is
		// carried over. A retry can go straight back into post-processing
		// without a download pass (every article already resolved), and
		// postproc's file list and QuickCheck read this name.
		fp.Filename = f.Filename
		// Same precedence as RestoreJobProgress, and for a sharper reason.
		// ResetForRetry clears exactly the articles whose failed bit is set,
		// so a complete file restored as all-done-none-failed leaves it with
		// nothing to reset: the rebuilt job looks fully downloaded and goes
		// straight back to post-processing, where it fails identically.
		// Retry would appear to run and change nothing (#300).
		s.decodeArticlesDone(f.ArticlesDone, job, f.FileIndex)
		if f.Complete {
			fp.Complete = true
		}
		if f.Deferred {
			fp.Deferred = true
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

// reconcileJobFiles finishes the write Store.ReplaceManifest was interrupted
// partway through, for a job whose stored rows and stored manifest describe
// different file sets.
//
// The blob is the newer artifact by construction: ReplaceManifest writes it
// before opening the transaction that rewrites job_files, and it is the only
// operation that changes a job's file set after Add. So when the two
// disagree, the manifest is the shape the discard intended and the rows are
// what did not get there.
//
// Rows are carried across by subject, not by index. The index is exactly what
// a renumber invalidates — that is the whole defect (#310) — while the subject
// identifies the file no matter where it moved. Matching on it preserves the
// per-file progress of every surviving file; rebuilding from the manifest
// alone would zero articles_done for all of them and re-download a job that
// was most of the way finished.
//
// A row with no match in the manifest is a file the discard dropped, and is
// not carried. job must be resident: the manifest being reconciled against is
// the one it holds.
func (s *SQLiteStore) reconcileJobFiles(ctx context.Context, job *Job) error {
	m, err := job.Manifest()
	if err != nil {
		return fmt.Errorf("sqlite store reconcile job_files %s: %w", job.ID, err)
	}
	stored, err := s.storedFileRows(ctx, job.ID)
	if err != nil {
		return fmt.Errorf("sqlite store reconcile job_files %s: %w", job.ID, err)
	}
	bySubject := make(map[string]storedFileRow, len(stored))
	for _, r := range stored {
		bySubject[r.subject] = r
	}

	// Rebuild progress at the manifest's indices first, then let
	// insertJobFilesTx write the rows from it. Going through the same writer
	// addTx and ReplaceManifest use is what keeps the three in agreement
	// about a row's full shape (#287).
	for i := range m.NumFiles() {
		r, ok := bySubject[m.FileSubject(i)]
		if !ok {
			continue
		}
		fp := &job.progress.files[i]
		fp.BytesDownloaded = r.bytesDownloaded
		fp.WriteCursor = r.writeCursor
		fp.AssembledCRC32 = r.assembledCRC32
		fp.Filename = r.filename
		fp.Complete = r.complete
		fp.Deferred = r.deferred
		s.decodeArticlesDone(r.articlesDone, job, i)
	}
	job.progress.recompute(m)

	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM job_files WHERE job_id = ?", job.ID); err != nil {
			return fmt.Errorf("sqlite store reconcile delete job_files %s: %w", job.ID, err)
		}
		return insertJobFilesTx(ctx, tx, job, m)
	})
}
