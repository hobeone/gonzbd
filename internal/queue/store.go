package queue

import (
	"context"

	"github.com/hobeone/gonzbd/internal/history"
)

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

	// NonResidentFieldsByJob returns every job's persisted download start,
	// download finish, and par2 release reason, decoded, in a single grouped
	// query. It is the sibling of ArticleCountsByJob and exists for the same
	// reason: Get restores these fields only on the resident branch, so Load
	// must supply them when it builds a non-resident job's JobProgress.
	// Without it a paused job's fields are dropped on load and then
	// overwritten with zeros by the next update.
	//
	// Membership follows one rule, the same one NonResidentFields itself
	// states: a column belongs here iff updateTx writes it from JobProgress.
	//
	// A job with none of these fields set is present in the map with all of
	// them zero, not absent — the caller routes them through the owner
	// either way, and an absent entry would be indistinguishable from a job
	// the query did not see.
	NonResidentFieldsByJob(ctx context.Context) (map[string]NonResidentFields, error)

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
