package job

import (
	"errors"
	"fmt"
	"slices"
)

// ErrIllegalTransition is returned when a requested state change is not an
// edge in the machine below.
var ErrIllegalTransition = errors.New("job: illegal state transition")

// door names which of the two moving doors may take an edge. It exists so that
// "which move is a boundary crossing" is written down once, in legalEdges,
// rather than re-derived by a zone predicate in transition and a literal state
// check in cross.
//
// That duplication was not theoretical. With the fact stored three times, the
// zone predicate ran before the graph was consulted, so every
// Correctness→Production PAIR answered ErrCrossRequired — including four that
// are not edges at all — and a caller that obeyed "call Cross instead" got
// ErrIllegalTransition from cross, which is legal only from Assessing.
type door uint8

const (
	// byTransition is the ordinary work-spine move.
	byTransition door = iota
	// byCross is the boundary crossing. Exactly one edge carries it, which
	// TestLegalEdgesIsTheWorkSpine asserts, and cross is its only door
	// because crossing also yields the lease.
	byCross
)

// legalEdges is the lifecycle as a directed graph: the six-edge WORK SPINE, and
// nothing else. Pause and resume are not edges — a job that is not running is
// still at the state it last occupied (design §3.2) — and cancellation is not
// an edge either, because finish is its own door and never consults this map
// (`sed -n '/func (a \*Attempt) finish/,/^}/p' internal/job/attempt.go | grep
// CanTransition` returns nothing).
//
// Each edge carries the door that may take it. Exactly one is byCross —
// Assessing → Extracting — which is why Cross owns one EDGE rather than a
// state class, and why neither door needs a zone predicate to route.
var legalEdges = map[State][]edge{
	Fetching:   {{Assessing, byTransition}},
	Assessing:  {{Fetching, byTransition}, {Repairing, byTransition}, {Extracting, byCross}},
	Repairing:  {{Assessing, byTransition}},
	Extracting: {{Finalizing, byTransition}},
	Finalizing: {},
	Finished:   {},
}

// edge is one entry in legalEdges: a destination and the door that may take it.
type edge struct {
	to   State
	door door
}

// edgeFrom resolves from → to in legalEdges. It is the single lookup both
// moving doors use, so "does this move exist" and "who may take it" are
// answered together and cannot disagree.
func edgeFrom(from, to State) (edge, bool) {
	i := slices.IndexFunc(legalEdges[from], func(e edge) bool { return e.to == to })
	if i < 0 {
		return edge{}, false
	}
	return legalEdges[from][i], true
}

// CanTransition reports whether a job may move from → to by ANY door. It says
// nothing about which one: Assessing → Extracting is an edge, and answering
// true here is what lets cross take it while transition refuses it.
//
// Self-transitions are NOT legal. The previous from == to early return existed
// partly to keep hold's Finalizing case reachable, and hold is gone; no door
// requests one now, and leaving it would permit a legal no-op that clears
// Activity and nothing else.
func CanTransition(from, to State) bool {
	_, ok := edgeFrom(from, to)
	return ok
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

// wrongDoor reports an edge that exists but belongs to the other door. It is
// the counterpart of ErrCrossRequired, which transition returns for the same
// situation in the other direction.
func wrongDoor(from, to State) error {
	return fmt.Errorf("%w: %s → %s is an edge, but transition takes it, not cross", ErrIllegalTransition, from, to)
}
