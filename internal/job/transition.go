package job

import (
	"errors"
	"fmt"
	"slices"
)

// ErrIllegalTransition is returned when a requested state change is not an
// edge in the machine below.
var ErrIllegalTransition = errors.New("job: illegal state transition")

// legalEdges is the lifecycle as a directed graph. 22 edges, and every one is
// reachable — there are no fan-out blocks, because "still producing, doing
// something else now" is an Activity write rather than a transition.
//
// Three shapes account for all of it:
//
//   - The work spine (8 edges): Waiting→Fetching, Fetching→Assessing,
//     Assessing→{Fetching, Repairing, Extracting, Finished},
//     Repairing→Assessing, Extracting→Finalizing, Finalizing→Finished.
//   - Pause (6 edges): every non-terminal state may enter Waiting, and
//     Waiting may re-enter any state that can be a StateView.Next.
//   - Cancel (6 edges): every non-terminal state may reach Finished.
//
// A self-transition is always legal and is treated as an idempotent no-op by
// CanTransition, so callers need not special-case it.
//
// The one edge the graph must NOT contain is any return from Production to
// Correctness. TestBoundaryIsOneWay enumerates AllStates() and fails if one
// appears, rather than trusting this comment.
var legalEdges = map[State][]State{
	Waiting:    {Fetching, Assessing, Repairing, Extracting, Finalizing, Finished},
	Fetching:   {Assessing, Waiting, Finished},
	Assessing:  {Fetching, Repairing, Extracting, Waiting, Finished},
	Repairing:  {Assessing, Waiting, Finished},
	Extracting: {Finalizing, Waiting, Finished},
	Finalizing: {Waiting, Finished},
	Finished:   {},
}

// CanTransition reports whether a job may move from → to. Self transitions
// are always legal.
func CanTransition(from, to State) bool {
	if from == to {
		return true
	}
	return slices.Contains(legalEdges[from], to)
}

// IsCorrectness reports whether s is in the reversible zone — the states whose
// goal is having the correct bytes, and which touch nothing outside the job's
// own working directory.
func IsCorrectness(s State) bool {
	return s == Fetching || s == Assessing || s == Repairing
}

// IsProduction reports whether s is past the irreversible boundary — the
// states that delete archives, move files and run user scripts.
func IsProduction(s State) bool {
	return s == Extracting || s == Finalizing
}

func illegalTransition(from, to State) error {
	return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, from, to)
}
