package sched

import (
	"errors"

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
		if job.IsProduction(s.State.State) {
			return nil // gate: let it reach the end; D-I11 lets it complete OK
		}
		q.work.Abort(j.ID()) // interrupt: settled on the tick after it yields
		return nil
	}
	l, err := j.Finish(job.OutcomeCancelled, q.now())
	if err != nil {
		// Finish refused: the attempt was NOT cancelled, so the job still
		// occupies its current position and needs whatever that position
		// requires. Releasing here would strand it resourceless while still
		// running — the same shape of bug B1's Retry fix guards against.
		return err
	}
	// Freed BEFORE the reclaim. reclaim can fail its identity audit, and an
	// earlier order returned through that failure with the slot still held —
	// turning one audit error into a permanent pool-B leak. This ordering is
	// unconditional only with respect to reclaim's outcome, not Finish's: the
	// guard above is what keeps it from running on a failed Finish.
	q.releaseFor(j, job.StateUnset)
	if rerr := q.reclaim(l); rerr != nil {
		// Both errors are real: Finish already succeeded (the job IS settled,
		// so the caller must not retry Finish), and reclaim's identity audit
		// caught something separately wrong with the lease. Neither may be
		// dropped in favor of the other.
		return errors.Join(err, rerr)
	}
	return err
}
