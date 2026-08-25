package job

import "fmt"

// WaitReason is why a job is held at a state boundary. Waiting for a lease,
// waiting for a compute slot, and being paused are the same situation — the
// job is at a known boundary, holds nothing, decides nothing, and is blocked
// on permission — so they are one state with a reason rather than three
// states (spec §8.2).
type WaitReason uint8

const (
	// NoLease means no acquisition lease is available. This is also the
	// reason a job that has never run is waiting.
	NoLease WaitReason = iota
	// NoComputeSlot means no compute slot is available.
	NoComputeSlot
	// UserPaused means this job was paused.
	UserPaused
	// GlobalPause means the whole queue is paused.
	GlobalPause
)

// AllWaitReasons returns every declared reason.
func AllWaitReasons() []WaitReason {
	return []WaitReason{NoLease, NoComputeSlot, UserPaused, GlobalPause}
}

func (r WaitReason) String() string {
	switch r {
	case NoLease:
		return "NoLease"
	case NoComputeSlot:
		return "NoComputeSlot"
	case UserPaused:
		return "UserPaused"
	case GlobalPause:
		return "GlobalPause"
	default:
		return fmt.Sprintf("WaitReason(%d)", uint8(r))
	}
}

// IsPause distinguishes "held because a person or the queue said stop" from
// "held because capacity is full". Only the former renders as Paused to the
// API; capacity waits render as Queued (spec §12).
func (r WaitReason) IsPause() bool {
	return r == UserPaused || r == GlobalPause
}

// StateView is the immutable read shape of a job's current attempt. It is what
// Job.State() returns and the only thing consumers outside this package see —
// no consumer holds a job lock, and no consumer reaches a mutable field.
//
// Next and Reason are meaningful only when State is Waiting. Activity is
// ActNone unless work is executing. Outcome is OutcomePending until the
// attempt reaches Finished.
//
// The zero value is a job that has never run: Waiting for a lease.
type StateView struct {
	State    State
	Next     State
	Reason   WaitReason
	Activity Activity
	Outcome  Outcome
	// Assessed reports whether this attempt has already been through
	// Assessing. It exists so ToSABnzbd can tell a first-pass download from a
	// re-entry that is fetching recovery volumes — which is exactly what
	// upstream's "Fetching" status means (spec §12).
	Assessed bool
}
