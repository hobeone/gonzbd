package job

import (
	"errors"
	"fmt"
	"slices"
)

// ErrIllegalTransition is returned when a requested state change is not an
// edge in the machine below.
var ErrIllegalTransition = errors.New("job: illegal state transition")

// legalEdges is the lifecycle as a directed graph: 22 edges, and every one is
// reachable — there are no fan-out blocks, because "still producing, doing
// something else now" is an Activity write rather than a transition.
//
// A hand-counted breakdown of the 22 by shape is exactly the kind of claim
// Standing Design Rule 4 forbids stating in prose: an earlier version of this
// comment recited "8 spine + 6 pause + 6 cancel", which both undercounts (the
// listed spine edges alone number nine) and double-counts (Assessing→Finished
// and Finalizing→Finished were claimed under both spine and cancel). Rather
// than recite a corrected set of numbers that can drift the same way, this
// comment states the partition RULE, and
// TestEdgeCountsMatchTheStatedPartition classifies every edge in the map
// below by it and asserts the bucket sizes and their total — so a change to
// legalEdges that shifts a count fails the test instead of leaving stale
// numbers here. Every edge falls into exactly one bucket, checked in this
// order:
//
//  1. Cancel — target is Finished (6 edges: one per non-terminal source).
//  2. Pause — target is Waiting (5 edges: one per non-Waiting, non-terminal
//     source).
//  3. Resume — source is Waiting, target is not Finished (5 edges: Waiting
//     may resume into any of the other five non-terminal states).
//  4. Work spine — everything else (6 edges): Fetching→Assessing,
//     Assessing→Fetching, Assessing→Repairing, Assessing→Extracting,
//     Repairing→Assessing, Extracting→Finalizing.
//
// Self-transitions are not in this graph at all: CanTransition's from == to
// early return reports every one of the seven states as legally
// self-transitioning, without a self-edge appearing in legalEdges. That is a
// property of CanTransition, not of the doors that actually drive a change:
// Attempt.transition rejects to == Waiting and to == Finished outright
// (ErrHoldRequired, ErrFinishRequired) before it ever compares a.state to
// to, so those two states can never actually be self-transitioned through
// transition, even though CanTransition(Waiting, Waiting) and
// CanTransition(Finished, Finished) both report true.
//
// One consequence of that self-transition legality is worth naming: it is
// what keeps the Finalizing → Waiting pause edge reachable at all.
// legalEdges[Finalizing] is {Waiting, Finished}, and hold validates a pause's
// next against CanTransition(a.state, next) after already excluding
// next == Finished (ErrFinishRequired) and next == Waiting (self-referential
// hold is refused). From Finalizing, the only next value left that
// CanTransition(Finalizing, next) accepts is Finalizing itself, via this
// same from == to return — so a Finalizing attempt can only ever pause to
// resume back into Finalizing, never into any other state.
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
