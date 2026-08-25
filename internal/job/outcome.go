package job

import "fmt"

// Outcome is an attempt's verdict. It is write-once: assigned only on the
// edge into Finished, and never revised.
//
// The old model made "did this job fail?" a question whose answer could
// change, because Failed → Queued was a legal edge. Retry therefore had to
// reconstruct "failed, retry me" from per-article bits. Here a retry appends
// a new Attempt with its own Outcome instead, so a verdict is superseded
// rather than mutated (spec §3.1).
type Outcome uint8

const (
	// OutcomePending means the attempt has not reached Finished. The zero
	// value, so an in-flight attempt carries it without an assignment.
	OutcomePending Outcome = iota
	// OutcomeOK means the job produced its files.
	OutcomeOK
	// OutcomeFailed means production ran and something in it failed.
	OutcomeFailed
	// OutcomeUnrecoverable means the verdict was Unrecoverable, so the job
	// never crossed the boundary. Its files are still in the working
	// directory and it is still retryable (D3) — which is the whole reason
	// this is a distinct outcome from OutcomeFailed rather than folded into
	// it.
	OutcomeUnrecoverable
	// OutcomeCancelled means a person stopped it.
	OutcomeCancelled
)

// AllOutcomes returns every declared outcome.
func AllOutcomes() []Outcome {
	return []Outcome{
		OutcomePending, OutcomeOK, OutcomeFailed,
		OutcomeUnrecoverable, OutcomeCancelled,
	}
}

func (o Outcome) String() string {
	switch o {
	case OutcomePending:
		return "Pending"
	case OutcomeOK:
		return "OK"
	case OutcomeFailed:
		return "Failed"
	case OutcomeUnrecoverable:
		return "Unrecoverable"
	case OutcomeCancelled:
		return "Cancelled"
	default:
		return fmt.Sprintf("Outcome(%d)", uint8(o))
	}
}

// IsSettled reports whether a verdict has been reached. Every value except
// OutcomePending is settled, and a settled outcome may never be reassigned.
func (o Outcome) IsSettled() bool {
	return o != OutcomePending
}
