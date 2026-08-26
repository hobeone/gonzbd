package job

import "fmt"

// State is the position of a job's current attempt in the lifecycle. It
// answers where the job is and what may happen next; what is executing right
// now is Activity, and how the attempt ended is Outcome. Keeping the three
// apart is what collapses the transition table from a fan-out into a graph —
// see docs/superpowers/specs/2026-08-25-job-lifecycle-design.md §3.
//
// The field lives on the current Attempt, not on the Job (§3.1).
type State uint8

const (
	// StateUnset is not a state. It is the zero value, and exists so that a
	// zero StateView cannot be mistaken for a job in a real state: without
	// it, removing Waiting would promote Fetching to zero and an
	// uninitialized view would read as an active download.
	//
	// No door accepts it as a destination — `grep -n 'StateUnset'
	// internal/job/transition.go` returns nothing, so it is neither a key
	// nor a value in legalEdges — and AllStates() does not list it, which
	// TestAllStates_Exhaustive asserts by name rather than by count.
	//
	// Job.State() does NOT yet return it for a job with no attempt; that
	// case is still Waiting{Next: Fetching}, constructed at job.go:120.
	StateUnset State = iota
	// Waiting holds no lease and no compute slot. It knows where it is going
	// (StateView.Next) and why it is held (StateView.Reason); it never
	// decides anything itself.
	Waiting
	// Fetching is downloading articles. Holds a lease.
	Fetching
	// Assessing decides whether the bytes are correct. Holds a lease and a
	// compute slot. Within the Correctness zone it is the only state with
	// more than one non-pause, non-cancel work successor — every other
	// Correctness state has at most one.
	// TestOnlyAssessingBranchesWithinCorrectness pins this for the
	// Correctness zone specifically; it does not examine Production
	// (Extracting, Finalizing), where the same property happens to hold
	// today but is unenforced — see doc.go.
	Assessing
	// Repairing runs par2 repair. Holds a lease and a compute slot.
	Repairing
	// Extracting decompresses archives. Holds a compute slot. First state
	// past the irreversible boundary.
	Extracting
	// Finalizing renames, cleans, moves and runs the user script. Holds a
	// compute slot.
	Finalizing
	// Finished is terminal. The attempt's Outcome is assigned on the edge
	// into it and never revised.
	Finished
)

// AllStates returns every declared State. TestAllStates_Exhaustive fails if
// this disagrees with the const block above, so a new state cannot be added
// without appearing here.
func AllStates() []State {
	return []State{
		Waiting,
		Fetching,
		Assessing,
		Repairing,
		Extracting,
		Finalizing,
		Finished,
	}
}

func (s State) String() string {
	switch s {
	case StateUnset:
		return "StateUnset"
	case Waiting:
		return "Waiting"
	case Fetching:
		return "Fetching"
	case Assessing:
		return "Assessing"
	case Repairing:
		return "Repairing"
	case Extracting:
		return "Extracting"
	case Finalizing:
		return "Finalizing"
	case Finished:
		return "Finished"
	default:
		return fmt.Sprintf("State(%d)", uint8(s))
	}
}
