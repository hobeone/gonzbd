package job

import "fmt"

// Intent is what a person has asked of this job, independent of where the job
// is (State), what it is executing (Activity), or how an attempt ended
// (Outcome). It is the fourth orthogonal axis — see
// docs/superpowers/specs/2026-08-26-lifecycle-intents-design.md §3.1.
//
// It exists because pause is a GATE, not an interrupt (prior spec §8.3): work
// in flight runs to the end of its state and the job then stops. The request
// and the boundary that consumes it are separated in time, and the request
// needs somewhere to live in between. Before this axis there was nowhere — a
// pause could only be recorded once the job had ALREADY stopped, which is the
// wrong way round and made "finishing repair, then pausing" unrepresentable.
//
// Intent lives on the Job, not the Attempt: a paused job that is retried stays
// paused, because the pause was a statement about the job rather than about
// one run of it.
type Intent uint8

const (
	// IntentRun is the default: nothing has been asked, so the job runs when
	// the scheduler can serve it.
	IntentRun Intent = iota
	// IntentPause means stop at the next gate. Freely reversible.
	IntentPause
	// IntentCancel means stop and settle. It LATCHES — see SetIntent.
	IntentCancel
)

// AllIntents returns every declared Intent. TestAllIntents_EveryEntryHasAStringArm
// fails if one lacks a String arm.
func AllIntents() []Intent { return []Intent{IntentRun, IntentPause, IntentCancel} }

// IsLatched reports whether this intent, once set, refuses to be replaced.
func (i Intent) IsLatched() bool { return i == IntentCancel }

func (i Intent) String() string {
	switch i {
	case IntentRun:
		return "IntentRun"
	case IntentPause:
		return "IntentPause"
	case IntentCancel:
		return "IntentCancel"
	default:
		return fmt.Sprintf("Intent(%d)", uint8(i))
	}
}
