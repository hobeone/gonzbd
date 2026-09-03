package job

import "fmt"

// WaitReason is why a job is not running. Waiting for a lease, waiting for a
// compute slot, and being paused are the same situation — the job holds
// nothing, decides nothing, and is blocked on permission — so they are one
// reason-carrying value rather than three states.
//
// They were once literally one State, called Waiting, with this as its
// reason. That state is gone: a job that cannot proceed now stays in its work
// state and is simply not running, and the reason is derived by the Queue
// rather than stored on the attempt — it reaches consumers as the Reason
// field on RenderView (render.go), not StateView. No non-test code in this
// package writes RenderView.Reason (`git grep -n -E '\bReaso[n]:' --
// 'internal/job/*.go' ':!internal/job/*_test.go'` returns nothing);
// sabnzbd_test.go builds RenderView literals that set it, which is how the
// render table is driven without a Queue to derive it. The word-boundary
// `\b` matters now that the content-tier move brought progress.go's
// `Par2ReleaseReason:` composite-literal key into this package — an
// unrelated string field on JobProgress whose name happens to share the
// same trailing letters and colon that this pattern looks for, which an
// unanchored version of it would also match.
type WaitReason uint8

const (
	// NoLease means no acquisition lease is available. It is the zero
	// WaitReason, which suits a job that is not running for want of one.
	// Note a never-run job no longer carries a reason of its own — with
	// Waiting removed there is no pre-attempt state to hold one, and
	// Job.State() reports StateUnset for that case.
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
// Next is meaningful when set, and means the current state's work has ended
// and the job continues to the named state (§3.3) — it is not, as under the
// old Waiting model, a destination waiting for permission to resume. The wait
// reason that used to live here is gone along with Waiting; RenderView (task
// 4) carries it instead, because it is derived by the Queue rather than
// stored on the attempt. Activity is ActNone unless work is executing.
// Outcome is OutcomePending until finish settles the attempt.
//
// The zero value is StateUnset in both State and Next. That is deliberately
// not any real state (see StateUnset in state.go), so an uninitialized view
// is inert rather than plausible. It is also exactly the shape Job.State()
// returns for a job that has never run — StateUnset IS the never-run shape,
// with no exception left to carve out: the old model's Waiting{Next:
// Fetching} answer claimed a position the job had not reached, and it is gone
// along with Waiting itself.
type StateView struct {
	State    State
	Next     State
	Activity Activity
	Outcome  Outcome
	// Assessed reports whether this attempt has already been through
	// Assessing. It exists so ToSABnzbd can tell a first-pass download from a
	// re-entry that is fetching recovery volumes — which is exactly what
	// upstream's "Fetching" status means (spec §12).
	Assessed bool
}
