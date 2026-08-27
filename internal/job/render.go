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
	Reason WaitReason
	// Intent is the Job's, carried here so a consumer can render "finishing
	// repair, then pausing" — a running job with IntentPause shows its state,
	// not Paused.
	Intent Intent
}
