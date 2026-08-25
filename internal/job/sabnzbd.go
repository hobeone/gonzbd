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
// distinction on its own. The translation is one-way: nothing reads a
// constants.Status back into the machine. That is the whole point of having
// a shim rather than storing the upstream vocabulary — see spec §12.
//
// It is total by construction: every State arm returns, and the Finished and
// Waiting arms delegate to helpers that also return on every input via a
// default case. TestToSABnzbd_IsTotal walks the product space of every axis
// to prove no combination yields an empty string, because an unhandled
// combination shows up as a blank status in somebody's Sonarr rather than as
// a crash here.
//
// Four upstream statuses that no code path of ours assigns are OUTPUTS here
// and nothing more: Grabbing and Checking are unreachable (nothing in this
// design corresponds to them — GoNZBD's Assessing state is what upstream
// calls verification, mapped below), Idle and Propagating are likewise never
// produced by this function. Fetching finally means what upstream documents
// it to mean — downloading extra par2 files for repair, which is exactly our
// Assessing → Fetching re-entry, told apart from a first-pass download by the
// attempt's latched Assessed flag.
func ToSABnzbd(v StateView) constants.Status {
	switch v.State {
	case Waiting:
		if v.Reason.IsPause() {
			return constants.StatusPaused
		}
		return constants.StatusQueued

	case Fetching:
		// A re-entry after a verdict is fetching recovery volumes. Anything
		// before the first assessment is an ordinary download.
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

	case Finished:
		return finishedStatus(v.Outcome)

	default:
		// Reachable only if AllStates() grows a value this switch does not
		// yet have an arm for — TestAllStates_Exhaustive (state_test.go) is
		// what would catch that omission at the State level; this default is
		// what keeps ToSABnzbd itself total in the meantime.
		return constants.StatusQueued
	}
}

// finishedStatus maps a Finished attempt's verdict to the status shown once a
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
