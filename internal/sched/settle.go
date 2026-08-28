package sched

import (
	"errors"

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

// errCancelReserved is Settle's refusal of an outcome only the cancel latch
// may produce.
var errCancelReserved = errors.New("sched: Settle: OutcomeCancelled is reserved for Cancel")

// Settle is the door a dispatcher calls when its worker has returned
// terminally. It is the counterpart of the three exported job doors that YIELD
// a lease — Cross, Finish and Surrender — none of which had an exported
// reclaimer before this: q.reclaim is unexported and is the sole reclaimer
// (§3.9, D-I13), so a dispatcher calling j.Finish itself would drop the lease
// and lose a pool-A slot permanently and silently.
//
// PRECONDITION, which this package cannot check: the caller's worker for j has
// returned and will not touch the job's lease, slot, manifest or barrier
// again. There is deliberately no q.running guard here, and its absence is not
// an oversight. running() is IsOpen && Next == StateUnset && holds(), and for
// a worker that has just finished normally at Fetching every conjunct is still
// TRUE — so a !running guard would reject exactly the call this door exists to
// serve. Cancel and Retry guard because a USER initiates them and does not
// know whether a worker is mid-article; here the caller IS the worker's owner,
// which is the evidence.
//
// It refuses OutcomeCancelled. Cancel is final for a Job (D-I14) and it is
// final because Cancel latches SetIntent(IntentCancel) before settling. A
// caller reaching this door with Cancelled would skip the latch, leaving a job
// that renders as Deleted (§4.4) while still carrying IntentRun — so q.Retry
// would see an ordinary settled attempt and reopen it. The refusal returns
// before q.mu is taken, because it is a pure check on an argument.
func (q *Queue) Settle(j *job.Job, o job.Outcome) error {
	if o == job.OutcomeCancelled {
		return errCancelReserved
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.settleLocked(j, o, j.Snapshot())
}
