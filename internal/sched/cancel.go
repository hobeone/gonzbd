package sched

import (
	"github.com/hobeone/gonzbd/internal/job"
)

// Cancel latches the job's intent and then settles it if nothing is in flight.
// Prior spec §8.4 makes cancel an interrupt before the boundary and a gate after it.
func (q *Queue) Cancel(j *job.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := j.SetIntent(job.IntentCancel); err != nil {
		return err
	}
	return q.finishCancel(j, j.Snapshot())
}

// finishCancel takes the snapshot its caller already read rather than reading
// again: two reads would reintroduce the tear job.Snapshot closes, at the one
// site where the intent has just changed underneath.
//
// The gate is IsProduction && running, NOT IsProduction && !workDone. §3.7
// lists three ways the latter fails, all because Finalizing never sets next so
// !workDone is permanently true there: a Finalizing job could never be
// cancelled; a post-boundary job restored from a restart gated forever; and a
// Finalizing job waiting for a slot gated forever with no work in flight.
func (q *Queue) finishCancel(j *job.Job, s job.Snapshot) error {
	if s.State.State == job.StateUnset {
		// A never-run job cannot be settled — Outcome lives on the Attempt and
		// there is none, so Finish returns ErrNoOpenAttempt. Cancelling a
		// queued job removes it from the queue, which is what a user means.
		// discard is Half B2's, with the store; naming it here keeps the case
		// from being silently unhandled.
		return nil
	}
	if s.State.Outcome.IsSettled() {
		// A settled attempt KEEPS the position it settled at (§3.3) but needs
		// none of that position's resources — the same rule Advance's own
		// settled branch applies. Without this release, a job that settles at
		// Assessing, Repairing, Extracting or Finalizing and is THEN cancelled
		// strands its slot forever: Advance routes s.Intent == IntentCancel to
		// finishCancel before it ever reaches its own settled-branch release,
		// so no later tick can recover what this arm fails to free.
		q.releaseFor(j, job.StateUnset)
		return nil // already closed, by cancel or otherwise
	}
	if q.running(j.ID(), s) {
		// A worker owns this job's resources and is using them. Neither arm
		// may seize a lease or slot out from under it.
		if !cancelInterrupts(s.State.State) {
			return nil // gate: let it reach the end; D-I11 lets it complete OK
		}
		q.work.Abort(j.ID()) // interrupt: settled on the tick after it yields
		return nil
	}
	// This line is reached both pre- and post-boundary: a non-running
	// post-boundary job — Extracting or Finalizing waiting on a compute slot,
	// or a job restored from a restart (§5.12) — has q.running == false and
	// falls through the two arms above to here, same as a pre-boundary one.
	// At post-boundary states cancelInterrupts is false, so settleLocked's own
	// override — its `s.Intent == job.IntentCancel && cancelInterrupts` check,
	// applied to the job's current state — does NOT fire there; passing
	// OutcomeCancelled explicitly is what makes a non-running post-boundary
	// job cancel at all; settleLocked would otherwise record whatever outcome
	// a caller happened to pass. Only for a pre-boundary job does
	// settleLocked's override make this argument redundant with what it would
	// have produced anyway.
	return q.settleLocked(j, job.OutcomeCancelled, s)
}
