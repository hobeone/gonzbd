package queue

import (
	"context"

	"github.com/hobeone/gonzbd/internal/history"
)

// FileMeta is the per-file shape Load needs to size a non-resident job's
// progress without a manifest. It carries Bytes, Complete, and Fetch
// alongside the article count for the bitsets.
type FileMeta struct {
	ArticleCount int
	Bytes        int64
	Complete     bool
	// Fetch is the file's download intent, restored from
	// job_files.fetch_policy. See FetchPolicy.
	Fetch FetchPolicy
	// IsPar2 marks the file as par2 — index or recovery volume — rather than
	// content. Classified from job_files.subject rather than read from a
	// column: is_par2_recovery flags only volumes, and the index is exactly
	// the case that matters here. See FileProgress.IsPar2.
	IsPar2 bool
	// BytesDownloaded and FailedBytes are what let a NON-RESIDENT job report
	// the same byte figures a resident one does. A resident job derives both
	// from its manifest and article bitmaps in JobProgress.recompute; a job
	// whose manifest has been evicted has neither, so without these it reports
	// zero failed bytes and an inflated remaining figure — the residency
	// parity TestRemainingBytes_IdenticalResidentAndNonResident exists to
	// forbid.
	//
	// Both come from job_files, each caching a sum over the same article
	// resolution and written by the same statement, so neither can be
	// persisted out of step with the rest of its row. FailedBytes has no other
	// home available: failed_articles records WHICH articles failed and never
	// how many bytes they were, and a permanently failed article never decodes
	// so no durable run covers it either.
	//
	// BytesDownloaded could be derived from the durability record and once was
	// read from it. That was the wrong QUANTITY rather than an unavailable
	// one. This field is compared against Bytes, the encoded NZB per-file
	// total, so it must count ENCODED bytes; a run's Length is the DECODED
	// payload an fsync proved. Seeding one from the other overstated a
	// non-resident job's remaining bytes by the encoding overhead.
	//
	// Both are caches and neither is authoritative: hydration runs
	// JobProgress.recompute, which ASSIGNS these figures from the manifest and
	// the bitmaps and so supersedes whatever was seeded here (S4).
	BytesDownloaded int64
	FailedBytes     int64
	// Done and Failed are the file's per-article resolved state, DERIVED from
	// durable_runs and failed_articles — the same resolution the byte figures
	// above sum over.
	//
	// They exist for the same reason those do, one level down. Without them a
	// non-resident job's JobProgress had every article Pending, so a restart
	// showed a half-downloaded Queued or Paused job with EVERY article
	// remaining until it was promoted — and after the byte figures were
	// cached, the two disagreed in kind: bytes right, article count wrong.
	//
	// Indexed FILE-LOCALLY, length ArticleCount, while a run and a
	// failed-article row both name articles GLOBALLY — SQLiteStore.fillResolution
	// converts between the two using the running sum of the article counts
	// above, which is how it manages without a manifest. Nil means the job had
	// nothing recorded, which reads as "nothing resolved": the safe direction
	// under S3.
	//
	// failed implies done, and this is where that matters most.
	// newJobProgressSized reads Failed[i] only inside the Done[i] branch, so a
	// failed article with Done clear would come back Pending and be re-fetched
	// on every restart, forever.
	//
	// Caches, not authorities, on the same terms as the byte figures:
	// hydration runs JobProgress.recompute, which re-derives everything from
	// the manifest and the live bitmaps (S4).
	Done   []bool
	Failed []bool
}

// Store defines the persistence and ordering interface for active download queue jobs.
// It manages live job rows in SQLite while immutable article manifests reside on disk.
type Store interface {
	// Add inserts a new active job into the store.
	Add(ctx context.Context, job *Job) error

	// Get retrieves an active job by its ID.
	Get(ctx context.Context, id string) (*Job, error)

	// List returns all active jobs ordered by sort_key ASC.
	List(ctx context.Context) ([]*Job, error)

	// Update modifies an existing active job's metadata.
	Update(ctx context.Context, job *Job) error

	// UpdateBatch atomically persists a slice of modified jobs in a single
	// underlying storage transaction.
	UpdateBatch(ctx context.Context, jobs []*Job) error

	// Remove deletes an active job and its child job_files records.
	Remove(ctx context.Context, id string) error

	// MoveToHistory atomically inserts an entry into the history store and deletes
	// the active job and its job_files within a single database transaction.
	MoveToHistory(ctx context.Context, job *Job, entry history.Entry) error

	// ExistsByName checks if an active job with the given name exists using an index.
	ExistsByName(ctx context.Context, name string) (bool, error)

	// ExistsByMD5 checks if an active job with the given MD5 exists using an index.
	ExistsByMD5(ctx context.Context, md5 string) (bool, error)

	// ShiftSortKey reorders a job to a new integer 0-based index position,
	// shifting adjacent rows inside a database transaction to maintain contiguous keys.
	ShiftSortKey(ctx context.Context, id string, newIndex int) error

	// Prune removes orphaned jobs or child files from the store.
	Prune(ctx context.Context) error

	// SetPaused sets the global queue paused state in queue_meta.
	SetPaused(ctx context.Context, paused bool) error

	// IsPaused reports whether the global queue is paused.
	IsPaused(ctx context.Context) (bool, error)

	// RestoreJobProgress loads per-file progress counters into job.progress for a resident job.
	RestoreJobProgress(ctx context.Context, job *Job) error

	// RestoreRetryProgress overlays a failed job's retained per-file
	// progress onto a job rebuilt from its NZB, reporting whether the
	// overlay was applied. Nothing retained, or a rebuilt manifest whose
	// shape does not match the retained bitmap, yields false with a nil
	// error and means "download from scratch".
	RestoreRetryProgress(ctx context.Context, job *Job) (bool, error)

	// ArticleCountsByJob returns every job's per-file FileMeta — article
	// count, byte size, bytes already downloaded, failed bytes, whether
	// the file is complete, and its FetchPolicy — in a single grouped
	// query, indexed by file_index within each job. Used by Load to size
	// a non-resident job's JobProgress without reading its manifest.
	ArticleCountsByJob(ctx context.Context) (map[string][]FileMeta, error)

	// RecordFailedArticles persists article indices that will never be
	// fetched. Idempotent per (jobID, artIdx).
	RecordFailedArticles(ctx context.Context, jobID string, artIdxs []int32) error

	// ClearFailedArticles removes every failed-article row for ONE job.
	//
	// Three callers, enumerated from `git grep -n ClearFailedArticles`. Two
	// are wholesale reversals of the above, resetting a job's articles WITHOUT
	// exception: Queue.Retry via Job.ResetForRetry, and
	// Application.RetryHistoryJob, which resets a rebuilt job outside the
	// queue and so has to reach this method itself. For both, the articles
	// whose failed bit is cleared are exactly this job's stored set, so a
	// whole-job delete is equivalent to clearing them one at a time.
	//
	// The third is not a reversal at all: Application.dropJobDurability
	// (durability.go) drops the rows of a job that is DEPARTING, alongside its
	// durable runs. There is no in-memory state left to stay level with, so
	// the whole-job form is the only sensible one there.
	//
	// It must be scoped to a job the caller is actually resetting: a sweep
	// over every job would resurrect the permanently failed articles of
	// every non-resident one as fetchable work.
	//
	// Queue.ClearAllEmitted used to be a third such site and is not one now —
	// JobProgress.resetForReload retains the failed bit of an article whose
	// file is already Complete (#426), so its in-memory reset is a strict
	// subset of the stored set and this method would over-delete. It uses
	// ClearFailedArticlesByIdx below.
	ClearFailedArticles(ctx context.Context, jobID string) error

	// ClearFailedArticlesByIdx removes the failed-article rows for exactly
	// artIdxs within ONE job. It is the precise inverse of
	// RecordFailedArticles, and is idempotent: an index with no stored row is
	// not an error.
	//
	// It exists because Queue.ClearAllEmitted resets only SOME of a job's
	// failed articles, so it must name them rather than clear the job. The
	// caller passes the articles it actually reset; anything omitted keeps
	// its row, which is what holds the stored set level with memory across a
	// restart.
	//
	// It enumerates what to REMOVE rather than what to keep, and that is
	// load-bearing rather than stylistic: a "delete everything except these"
	// form cannot be chunked. Split across two statements, the second chunk's
	// NOT IN would delete the rows the first chunk was keeping.
	ClearFailedArticlesByIdx(ctx context.Context, jobID string, artIdxs []int32) error

	// DeleteJobArtifacts removes the on-disk manifest for job id
	// (manifests/<id>.json.gz). A missing file is not an error.
	//
	// It also removed progress/<id>.json.gz until #298. Nothing ever wrote
	// that file — it was left over from a pre-SQLite layout — so the removal
	// changed no behaviour.
	//
	// Callers must only invoke this after id has already left the queue's
	// in-memory index (q.byID) and its row(s) in the jobs/job_files tables —
	// never before or concurrently with that removal. Doing so earlier is
	// exactly the race this method exists to avoid: Queue.SnapshotJob clones
	// a job under q.mu.RLock, releases the lock, and only then hydrates its
	// manifest/progress from disk outside the lock. If the on-disk artifacts
	// are unlinked while the job is still reachable through q.byID, a
	// concurrent SnapshotJob can observe the job present but its manifest
	// already gone.
	//
	// SnapshotJob, not Snapshot: the plural form stopped hydrating and says
	// so at its own declaration.
	//
	// SnapshotJob is the only reader this ordering PROTECTS, not the only one
	// that reads a manifest unlocked — Queue.PromoteNext does too, and
	// defends itself instead by re-checking q.byID after re-acquiring the
	// lock and skipping a job that vanished meanwhile.
	//
	// Returns an error (rather than swallowing it internally) so callers and
	// tests can observe a failed unlink instead of the method silently doing
	// nothing. That said, deletion here is best-effort cleanup: the durable
	// truth of "does this job still exist" is the jobs/job_files rows and
	// q.byID, not these blobs, so callers are expected to log a returned
	// error and continue rather than fail the surrounding operation on it —
	// see Queue.Remove's use of this method.
	DeleteJobArtifacts(ctx context.Context, id string) error
}
