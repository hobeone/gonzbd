package queue

import (
	"context"

	"github.com/hobeone/gonzbd/internal/history"
)

// FileMeta is the per-file shape Load needs to size a non-resident job's
// progress without a manifest. It carries everything RemainingBytes reads
// — Bytes, BytesDownloaded, FailedBytes, Complete, and Deferred — alongside
// the article count for the bitsets, so a job reports the same remaining
// figure whether it is reconstructed resident (via the manifest) or
// non-resident (via this type).
type FileMeta struct {
	ArticleCount    int
	Bytes           int64
	BytesDownloaded int64
	// FailedBytes is the sum of bytes belonging to this file's permanently
	// failed articles, restored from job_files.failed_bytes — see
	// FileProgress.FailedBytes for why a non-resident job needs this
	// carried explicitly rather than recomputed from article bitmaps.
	FailedBytes int64
	Complete    bool
	Deferred    bool
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

	// ReplaceManifest rewrites job's stored manifest and replaces its
	// job_files rows to match the new file set. It requires job to be
	// resident, since the manifest being written is the one it holds.
	//
	// The two writes are one operation on purpose. Doing either alone is
	// what #294 was: the manifest and the rows must always describe the
	// same file set, and dropping a file renumbers every file_index after
	// it, so both have to move together.
	//
	// Two callers, with different synchronization. DiscardDeferredPar2 — the
	// one mutation that changes a job's files after Add — calls it under
	// q.mu, so the in-memory and persisted file sets are never observable
	// apart. Queue.reconcileJobFiles calls it outside q.mu on a
	// snapshot, to reconcile a job the first call left unpersisted (#310);
	// it accepts a wider window because there is already a disagreement to
	// close rather than one to prevent.
	//
	// The two cannot race on one job: DiscardDeferredPar2 self-gates on
	// having deferred files to discard, which is false once the first call
	// has stripped them.
	ReplaceManifest(ctx context.Context, job *Job) error

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
	// count, byte size, bytes already downloaded, failed bytes, and
	// whether the file is complete or deferred — in a single grouped
	// query, indexed by file_index within each job. Used by Load to size
	// a non-resident job's JobProgress without reading its manifest.
	ArticleCountsByJob(ctx context.Context) (map[string][]FileMeta, error)

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
