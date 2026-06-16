package queue

import (
	"errors"
	"fmt"
	"slices"

	"github.com/hobeone/gonzbd/internal/constants"
)

// ErrIllegalStatusTransition is returned by SetStatus/SetStatusIf when a
// requested job status change is not a legal edge in the lifecycle
// (FR-QUEUE-2).
var ErrIllegalStatusTransition = errors.New("queue: illegal status transition")

// legalStatusEdges is the job lifecycle as a directed graph (spec §4.3
// statuses). A self-transition (from == to) is always legal and treated
// as a no-op by callers. The graph is permissive within the real
// download → post-process → terminal flow; later phases must drive
// transitions along these edges.
//
// Notably: Failed → Queued enables retry; Deleted is terminal; the
// post-processing statuses fan out so the post-processor can record
// whichever stage it reaches.
var legalStatusEdges = map[constants.Status][]constants.Status{
	constants.StatusQueued: {
		constants.StatusDownloading, constants.StatusPaused, constants.StatusPropagating,
		constants.StatusFetching, constants.StatusFailed, constants.StatusDeleted,
	},
	constants.StatusDownloading: {
		constants.StatusQueued, constants.StatusPaused, constants.StatusFetching,
		constants.StatusVerifying, constants.StatusQuickCheck,
		constants.StatusFailed, constants.StatusDeleted,
	},
	constants.StatusPaused: {
		constants.StatusQueued, constants.StatusDownloading,
		constants.StatusFailed, constants.StatusDeleted,
	},
	constants.StatusPropagating: {
		constants.StatusQueued, constants.StatusDownloading,
		constants.StatusPaused, constants.StatusDeleted,
	},
	constants.StatusFetching: {
		constants.StatusDownloading, constants.StatusQueued,
		constants.StatusVerifying, constants.StatusFailed, constants.StatusDeleted,
	},
	constants.StatusVerifying: {
		constants.StatusQuickCheck, constants.StatusRepairing, constants.StatusExtracting,
		constants.StatusMoving, constants.StatusRunning, constants.StatusCompleted,
		constants.StatusFailed, constants.StatusDeleted,
	},
	constants.StatusQuickCheck: {
		constants.StatusVerifying, constants.StatusRepairing, constants.StatusExtracting,
		constants.StatusMoving, constants.StatusRunning, constants.StatusCompleted,
		constants.StatusFailed, constants.StatusDeleted,
	},
	constants.StatusRepairing: {
		constants.StatusVerifying, constants.StatusExtracting, constants.StatusMoving,
		constants.StatusRunning, constants.StatusCompleted, constants.StatusFailed, constants.StatusDeleted,
	},
	constants.StatusExtracting: {
		constants.StatusRepairing, constants.StatusMoving, constants.StatusRunning,
		constants.StatusCompleted, constants.StatusFailed, constants.StatusDeleted,
	},
	constants.StatusMoving: {
		constants.StatusRunning, constants.StatusCompleted,
		constants.StatusFailed, constants.StatusDeleted,
	},
	constants.StatusRunning: {
		constants.StatusCompleted, constants.StatusFailed, constants.StatusDeleted,
	},
	constants.StatusCompleted: {constants.StatusDeleted},
	constants.StatusFailed:    {constants.StatusQueued, constants.StatusDeleted},
	constants.StatusDeleted:   {},
	constants.StatusIdle:      {constants.StatusQueued},
}

// canTransitionStatus reports whether a job may move from → to. Self
// transitions are always legal (idempotent no-ops).
func canTransitionStatus(from, to constants.Status) bool {
	if from == to {
		return true
	}
	return slices.Contains(legalStatusEdges[from], to)
}

// illegalTransition builds the wrapped sentinel error for a rejected edge.
func illegalTransition(from, to constants.Status) error {
	return fmt.Errorf("%w: %s → %s", ErrIllegalStatusTransition, from, to)
}
