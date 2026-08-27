package job

import "github.com/hobeone/gonzbd/internal/constants"

// ToSABnzbd translates our internal machine into the legacy status vocabulary
// the /api?mode=queue contract exposes to third-party clients.
//
// This is the only NON-TEST file in the package that imports
// internal/constants — TestOnlyOneNonTestFileImportsConstants
// (sabnzbd_test.go) is what checks that, not this comment. sabnzbd_test.go
// itself also imports internal/constants (it needs constants.Status to
// write its table), which is exactly why the check is scoped to non-test
// sources rather than claiming to be the only importer full stop; `go list
// -deps` cannot see test files at all, so it could never have caught that
// distinction on its own. ToSABnzbd's own translation is one-way: it never
// reads a constants.Status back into the machine. That is the whole point of
// having a shim rather than storing the upstream vocabulary — see spec §12;
// this package makes no claim about whether some other, unrelated code in
// the repository ever converts a constants.Status back.
//
// ToSABnzbd takes a RenderView, not a StateView: Running and Reason are facts
// only a Queue can supply (§4.4), and this package has none yet — Half A
// constructs them directly in tests. A settled attempt short-circuits to
// finishedStatus regardless of Running; everything else is keyed on Running
// first, then on Reason while not running, then on State while running, with
// a default case at each switch. TestToSABnzbd_IsTotal walks the product
// space of every axis to prove no combination yields an empty string,
// because an unhandled combination shows up as a blank status in somebody's
// Sonarr rather than as a crash here.
//
// Four upstream statuses — Idle, Grabbing, Propagating and Checking — are
// statuses this function never produces: TestToSABnzbd_NeverEmitsUnproducedStatuses
// walks the same product space and would fail the moment any of the four
// appeared in ToSABnzbd's output. (TestToSABnzbd_EmitsOnlyDeclaredStatuses
// cannot make this claim — it checks membership in constants.AllStatuses(),
// which declares all four, so it would pass even if ToSABnzbd started
// emitting one of them.) They stay unreachable because nothing in this
// design corresponds to them: GoNZBD's Assessing state is what upstream
// calls verification (mapped below to Verifying/QuickCheck, never to
// Checking), and Grabbing, Idle and Propagating have no GoNZBD analogue at
// all. Fetching finally means what upstream documents it to mean —
// downloading extra par2 files for repair, which is exactly our Assessing →
// Fetching re-entry, told apart from a first-pass download by the attempt's
// latched Assessed flag.
func ToSABnzbd(v RenderView) constants.Status {
	// Settledness is an Outcome fact, not a State one. There is no Finished
	// state to key on: a settled attempt keeps the position it settled at, so
	// asking State here would have to enumerate every position instead of
	// asking the one axis that actually records the verdict.
	if v.Outcome.IsSettled() {
		return finishedStatus(v.Outcome)
	}
	if !v.Running {
		// Keyed on the wait REASON, not on Intent. Under a queue-wide pause
		// every job still carries IntentRun, so keying on IntentPause alone
		// would render the whole queue as Queued — a live API regression
		// against TestToSABnzbd_GlobalPauseRendersAsPaused. WaitReason.IsPause()
		// covers UserPaused and GlobalPause both, so routing through it cannot
		// omit one.
		if v.Reason.IsPause() {
			return constants.StatusPaused
		}
		return constants.StatusQueued
	}

	// Running. Intent is deliberately NOT consulted: a job with a pause
	// requested is still repairing until it reaches a gate, and reporting it
	// as Paused is what design §1.1 exists to prevent. Surfacing "finishing
	// repair, then pausing" is the UI reading RenderView.Intent alongside this
	// status.
	switch v.State {
	case Fetching:
		if v.Assessed {
			return constants.StatusFetching
		}
		return constants.StatusDownloading
	case Assessing:
		if v.Activity == ActCRCCheck {
			return constants.StatusQuickCheck
		}
		return constants.StatusVerifying
	case Repairing:
		return constants.StatusRepairing
	case Extracting:
		return constants.StatusExtracting
	case Finalizing:
		if v.Activity == ActScript {
			return constants.StatusRunning
		}
		return constants.StatusMoving
	default:
		// StateUnset with Running true is not constructible by the Queue — a
		// job with no attempt holds nothing — but ToSABnzbd is total by
		// construction and a blank status in somebody's Sonarr is worse than
		// a wrong-but-declared one.
		return constants.StatusQueued
	}
}

// finishedStatus maps a settled attempt's verdict to the status shown once a
// job leaves the queue. o is normally settled (finish rejects OutcomePending
// and any value AllOutcomes() does not declare — see Attempt.finish), but
// TestToSABnzbd_IsTotal constructs StateView values directly rather than
// through the machine, so this still needs an answer for OutcomePending and
// for any out-of-range Outcome; Failed is the safe direction for both, since
// it never reports success for a verdict we cannot read.
func finishedStatus(o Outcome) constants.Status {
	switch o {
	case OutcomeOK:
		return constants.StatusCompleted
	case OutcomeCancelled:
		return constants.StatusDeleted
	case OutcomeFailed, OutcomeUnrecoverable, OutcomePending:
		return constants.StatusFailed
	default:
		return constants.StatusFailed
	}
}
