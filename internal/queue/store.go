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

	// ArticleCountsByJob returns every job's per-file article counts in a
	// single grouped query, indexed by file_index within each job. A job
	// whose counts are all zero predates the article_count column, so the
	// caller must fall back to that job's manifest rather than sizing
	// progress to zero.
	ArticleCountsByJob(ctx context.Context) (map[string][]int, error)

	// RemainingBytesByJob returns each job's remaining bytes (manifest bytes
	// minus bytes already downloaded), summed per job_id across job_files.
	// Used by Loader.Load to seed JobProgress.remainingBytes for jobs that
	// come back from the store non-resident (no manifest, so no per-article
	// byte breakdown is available to compute this the normal way).
	RemainingBytesByJob(ctx context.Context) (map[string]int64, error)

	// DeleteJobArtifacts removes the on-disk manifest and progress files for
	// job id (manifests/<id>.json.gz and progress/<id>.json.gz). A missing
	// file is not an error.
	//
	// Callers must only invoke this after id has already left the queue's
	// in-memory index (q.byID) and its row(s) in the jobs/job_files tables —
	// never before or concurrently with that removal. Doing so earlier is
	// exactly the race this method exists to avoid: Queue.Snapshot clones a
	// job under q.mu.RLock, releases the lock, and only then hydrates its
	// manifest/progress from disk outside the lock. If the on-disk artifacts
	// are unlinked while the job is still reachable through q.byID, a
	// concurrent Snapshot can observe the job present but its manifest
	// already gone.
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
