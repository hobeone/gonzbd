package job

// RenderView is a job's state as a CONSUMER sees it: the attempt's own view,
// plus the three facts only the Queue can supply.
//
// Running-ness and the wait reason are DERIVED, never stored (design §3.4,
// D-I4). A job is running when its attempt is open, it holds everything its
// current state requires, and that state's work has not ended. Nothing in this
// package can answer that — it depends on pool-B slots and on a queue-wide
// pause flag that live in the Queue — so this type is the seam.
//
// Half A constructs these directly in tests, which is what keeps §4.4's whole
// table covered before a Queue exists. Half B fills them for real.
type RenderView struct {
	StateView

	// Running is the design's running(j): attempt open, holds what the
	// current state requires, and next unset.
	Running bool
	// Reason is why it is not running, and is meaningless when Running is
	// true. Derived by the Queue from intent, its own pause flag, and what
	// the job holds.
	//
	// Its zero value, NoLease, is ambiguous by construction: waitReason
	// (internal/sched/queue.go) returns (0, false) — not (NoLease, true) —
	// for a settled attempt, a running job, and a job whose work has ended
	// while already holding what the next state requires, and Render (Queue,
	// render.go) discards that boolean when it fills this field. So Reason ==
	// NoLease reads identically whether the job is genuinely waiting on pool
	// A or is one of those three "waiting for nothing" cases. Consult Running
	// first; do not read Reason == NoLease as "waiting for a lease" on its
	// own. Today's only consumer, ToSABnzbd, happens not to care — it checks
	// Reason.IsPause(), and NoLease is never a pause reason — but a future
	// consumer that branches on NoLease specifically would need Running
	// checked first for the same reason.
	Reason WaitReason
	// Intent is the Job's, carried here so a consumer can render "finishing
	// repair, then pausing" — a running job with IntentPause shows its state,
	// not Paused.
	Intent Intent
}
