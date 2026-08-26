package job

import (
	"errors"
	"fmt"
	"slices"
)

// ErrIllegalTransition is returned when a requested state change is not an
// edge in the machine below.
var ErrIllegalTransition = errors.New("job: illegal state transition")

// legalEdges is the lifecycle as a directed graph: the six-edge WORK SPINE, and
// nothing else. Pause and resume are not edges — a job that is not running is
// still at the state it last occupied (design §3.2) — and cancellation is not
// an edge either, because finish is its own door and never consults this map
// (`sed -n '/func (a \*Attempt) finish/,/^}/p' internal/job/attempt.go | grep
// CanTransition` returns nothing).
//
// Exactly one of these crosses the irreversible boundary: Assessing →
// Extracting. That is why Cross owns one EDGE rather than a state class.
var legalEdges = map[State][]State{
	Fetching:   {Assessing},
	Assessing:  {Fetching, Repairing, Extracting},
	Repairing:  {Assessing},
	Extracting: {Finalizing},
	Finalizing: {},
	Finished:   {},
}

// CanTransition reports whether a job may move from → to.
//
// Self-transitions are NOT legal. The previous from == to early return existed
// partly to keep hold's Finalizing case reachable, and hold is gone; no door
// requests one now, and leaving it would permit a legal no-op that clears
// Activity and nothing else.
func CanTransition(from, to State) bool {
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
