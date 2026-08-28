package sched

import (
	"github.com/hobeone/gonzbd/internal/job"
)

// cancelInterrupts reports whether Cancel stops this job's work rather than
// gating it. Prior spec §8.4 makes cancel an interrupt before the irreversible
// boundary and a gate after it, and this is that split, named once.
//
// It exists because two places need it and they must not disagree:
// finishCancel picks its arm on it, and settleLocked decides on it whether a
// cancelled job's outcome is OUR artifact or the worker's own. Written inline
// at both sites it would be one invariant in two files with nothing linking
// them — correct today only because the two copies happen to agree, and
// silently divergent the moment either moves. `grep -n 'cancelInterrupts('
// internal/sched/*.go | grep -v _test.go` finds four lines: the command
// documentation, this definition, and its two readers.
//
// The predicate is the complement of IsProduction rather than a list of
// states, because that is what §8.4's boundary is. Note IsProduction exists to
// answer a DIFFERENT question — whether a job may return to Fetching (§4.1's
// one-way boundary) — and cancel borrows it. The two happen to want the same
// line today; if §8.4's line ever moves (making Extracting interruptible, say),
// this function is the one place that changes.
func cancelInterrupts(s job.State) bool { return !job.IsProduction(s) }

// settleLocked is the SOLE settle path: it is the only code in this package
// that calls j.Finish. `grep -n 'j\.Finish(\|\.Finish(' internal/sched/*.go |
// grep -v _test.go` finds two lines: the command documentation and the call
// below. The caller must already
// hold q.mu, and must pass the snapshot it already read rather than letting
// this take a second one — two reads would reintroduce the tear job.Snapshot
// closes, at the one site where the intent may have just changed underneath.
//
// The order of the four steps is not arbitrary and each was fixed once during
// Half B1 review:
//
//  1. Apply the cancel latch, BEFORE Finish, because Finish writes the outcome
//     and there is no second chance to correct it.
//  2. Finish, and return on its error WITHOUT releasing anything — a refused
//     Finish means the attempt was not settled, so the job still occupies its
//     position and needs what that position requires. Releasing here would
//     strand it resourceless while still running.
//  3. Release the compute slot BEFORE the reclaim, because reclaim can fail
//     its identity audit and an earlier order returned through that failure
//     with the slot still held — turning one audit error into a permanent
//     pool-B leak.
//  4. Reclaim the lease.
func (q *Queue) settleLocked(j *job.Job, o job.Outcome, s job.Snapshot) error {
	// A cancelled job that was INTERRUPTED settles Cancelled whatever its
	// worker reported: we aborted it, so the worker's error is our own
	// artifact and recording it would be false. A cancelled job that was
	// GATED settles what actually happened — D-I11's running-Finalizing job
	// completes as OutcomeOK, because the files moved and the script ran.
	// Intent survives on the settled job either way, for the UI to read.
	if s.Intent == job.IntentCancel && cancelInterrupts(s.State.State) {
		o = job.OutcomeCancelled
	}
	l, err := j.Finish(o, q.now())
	if err != nil {
		return err
	}
	q.releaseFor(j, job.StateUnset)
	return q.reclaim(l)
}
