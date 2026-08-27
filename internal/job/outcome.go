package job

import "fmt"

// Outcome is an attempt's verdict. It is write-once: assigned only on the
// call that settles the attempt, and never revised.
//
// The old model made "did this job fail?" a question whose answer could
// change, because Failed → Queued was a legal edge. Retry therefore had to
// reconstruct "failed, retry me" from per-article bits. Here a retry appends
// a new Attempt with its own Outcome instead, so a verdict is superseded
// rather than mutated (spec §3.1).
type Outcome uint8

const (
	// OutcomePending means the attempt has not settled. The zero
	// value, so an in-flight attempt carries it without an assignment.
	OutcomePending Outcome = iota
	// OutcomeOK means the job produced its files.
	OutcomeOK
	// OutcomeFailed means the attempt settled having failed. It is
	// the typical case for a Production-stage failure, but finish does not
	// enforce IsProduction(state) for this outcome the way it does for
	// OutcomeUnrecoverable below — a Correctness-stage giving-up (e.g.
	// exhausting a policy's repair attempts) is also recorded here, and
	// nothing in this package currently distinguishes the two by state.
	OutcomeFailed
	// OutcomeUnrecoverable means the verdict was Unrecoverable, so the job
	// never crossed the boundary. Its files are still in the working
	// directory and it is still retryable (D3) — which is the whole reason
	// this is a distinct outcome from OutcomeFailed rather than folded into
	// it. finish rejects assigning this outcome to an attempt that has
	// crossed into Production — admissibleAt's row for it names the
	// Correctness zone, and inadmissible() returns
	// ErrUnrecoverableAfterBoundary; see admissibility.go — so this sentence
	// is enforced rather than only stated. The position it reads is
	// IsProduction(a.state), which was once distinct from a stored latch:
	// only a latch could still answer once finish had erased the position,
	// and change 03 stopped the erasure and collapsed them back into one.
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
