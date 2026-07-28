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
}
