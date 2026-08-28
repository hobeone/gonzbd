package sched

import (
	"errors"

	"github.com/hobeone/gonzbd/internal/job"
)

// errNotSettled is Retry's refusal for a job it must not touch: one whose
// current attempt is still open (a live worker holds its slot or lease) or
// which has never run at all. Both share one check because a never-run job's
// Outcome is the zero value OutcomePending, which IsSettled() already reports
// false — Retry's contract is "reopen a settled attempt", and neither shape
// is one.
var errNotSettled = errors.New("sched: Retry: attempt is not settled")

// Park is the door a dispatcher calls when its worker has stopped without
// finishing the state's work — the `yielded` report spec §3.6 names, whose
// handoff is "a worker that stops on a gate without ending its work reports
// yielded, and the dispatcher calls q.park(j)".
//
// PRECONDITION, which this package cannot check: the caller's worker for j has
// returned and will not touch the job's lease, slot, manifest or barrier
// again. running() stays TRUE for a worker that has yielded and not yet been
// parked — that is precisely why this door exists — so the fact is the
// caller's to guarantee, exactly as Workers.Abort's non-blocking requirement
// is.
//
// It is UNCONDITIONAL and takes no view on why the worker stopped. A gate is
// the common reason but not the only one: teardown, shutdown, and a connection
// that died all end a worker without ending the work, and all want this door.
// Gating it on gatedBy would refuse those while protecting nothing, since
// gatedBy reads Intent and q.paused and consults nothing about worker
// liveness.
//
// Advance cannot do this job. Its branch 2 tests q.holds BEFORE q.gatedBy and
// returns early for a job that still holds, because the Queue cannot tell
// "holding and working" from "holding and yielded" and stripping a live
// worker is the worse failure. So a gated job holding a lease mid-state is
// never parked by any number of Advance ticks; only this door releases it.
//
// It takes q.mu and delegates to parkLocked, which mutates both pools with no
// lock of its own. It returns an error because reclaim carries an identity
// audit that fails on a lease this pool did not issue or already took back —
// a real condition whose only other outlet would be silence. (§10's revision
// history records park's signature being narrowed to return nothing, on the
// grounds that the error was always nil; the audit added in Half B1 makes that
// no longer true.)
func (q *Queue) Park(j *job.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.parkLocked(j)
}

// parkLocked is Park's body, for a caller that already holds q.mu.
//
// `grep -n 'q\.parkLocked(' advance.go` finds exactly three lines: Park's own
// delegation, and Advance's branch 2 and branch 3 gated arms. Advance is the
// sole PRODUCTION caller that reaches it without going through park — the two
// arms sit inside Advance's own q.mu span, which is the whole reason this split
// exists. The qualifier is load-bearing: advance_test.go calls parkLocked
// directly, so an unqualified "sole caller" is false, and that pattern above
// searches advance.go only.
// An earlier version of this comment said "Advance is the sole production
// caller" and stated two lines, which was wrong the moment park was written to
// delegate here.
func (q *Queue) parkLocked(j *job.Job) error {
	q.releaseFor(j, job.StateUnset)
	return q.reclaim(j.Surrender())
}

// grantFor acquires what s requires and j does not already hold. Acquisition
// happens ONLY here: `grep -n 'leases\.issue' internal/sched/*.go | grep -v
// _test.go` finds exactly one line, the call below. `grep -n 'slots\.acquire'
// internal/sched/*.go | grep -v _test.go` finds two — that same call and
// queue.go's own citation of this name in prose — so one production call
// site for each pool, both inside this function's body.
//
// It grants the lease before the slot and does not roll back a lease it took
// when the slot is unavailable: the job keeps the lease and waits, which is
// what §3.4 means by "a job re-entering Fetching still holds its lease, so it
// is running by construction". Rolling back would make a job that is one slot
// short give up capacity it will need again on the next tick.
func (q *Queue) grantFor(j *job.Job, s job.State) bool {
	if needsLease(s) && !j.HoldsLease() {
		l := q.leases.issue()
		if l == nil {
			return false
		}
		if err := j.Grant(l); err != nil {
			// Grant's five refusals: nil, an unidentified lease, no open
			// attempt, an attempt past the boundary, or a second lease. The
			// pool never issues the first two. This is a claim about
			// grantFor's PRODUCTION call paths, not about grantFor itself:
			// `grep -n 'q\.grantFor(' advance.go | grep -v '// '` (the
			// second filter drops this comment's own mention of the name)
			// finds exactly three — branch 2 (line 200) and branch 3's two
			// calls (lines 219, 222). At all three, Advance's settled early
			// return has already established an open, unsettled attempt
			// before grantFor runs, so "no open attempt" and "past the
			// boundary" cannot fire from any of them; and at line 219 (the
			// crossing's ignored-result call), needsLease(s) is false for
			// both Production states, so this whole block is never entered
			// there at all. That leaves the fifth as the only refusal a production
			// call can hit: the job acquired a lease between our HoldsLease
			// check and here. Return the one we minted rather than leaking
			// it.
			//
			// grantFor has no such guarantee on its own — it is a package-
			// private function, and TestGrantFor_ReturnsIssuedLeaseOnGrantFailure
			// calls it directly on a job that never began an attempt, to
			// deliberately trigger the third refusal this comment says
			// cannot happen "here". "Here" means through Advance; it does
			// not mean grantFor is guarded against being called any other
			// way.
			_ = q.reclaim(l)
			return false
		}
	}
	if needsSlot(s) && !q.slots.acquire(j.ID()) {
		return false
	}
	return true
}

// Retry reopens a settled attempt. It is the door spec §5.9 and §5.10 name in
// their traces (`q.Retry(j) → BeginAttempt(now)`), and two comments in this
// package already point callers at it; without it those traces have no entry
// point and scenarios 5.9 and 5.10 cannot be written.
//
// It is deliberately NOT something Advance decides. A scheduling tick must
// never resurrect a job the user has not asked to resurrect, which is why
// Advance returns early on a settled attempt instead.
//
// It takes NO lease. BeginAttempt needs none (D-I12), and demanding one is the
// exact defect revision 3 shipped: §5.9 records a retry dropped permanently
// because the lease could not be taken and nothing recorded that a retry was
// wanted. Branch 2 grants on a later tick.
//
// It releases the settled attempt's compute slot BEFORE opening the new one.
// The new attempt starts at Fetching, which needsSlot == false (§3.4), so
// nothing on the Fetching path ever calls releaseFor again — without this,
// a job that last settled at Assessing, Repairing, Extracting or Finalizing
// carries that slot into the retried download and holds pool-B capacity for
// the whole re-fetch.
//
// It refuses with errNotSettled rather than releasing anything when the
// attempt is not settled — a live worker at Assessing, Repairing, Extracting
// or Finalizing, or a job that has never run. j.BeginAttempt already no-ops
// silently on an open attempt (job.go: `if a != nil && a.isOpen() { return
// nil }`), which made the release above unconditional: it ran, then
// BeginAttempt quietly declined to open anything, leaving the running
// worker's slot deleted from q.slots.held while the worker kept using it —
// pool B overcommitted past slotCap, and q.holds/q.running then read false
// for a job that was, in fact, still running. An error is the more honest
// signal here than BeginAttempt's silent no-op: calling Retry on a job that
// is not settled is a caller mistake (the door §5.9/§5.10 name is for a
// SETTLED attempt), not a condition a scheduling tick can quietly absorb the
// way an idempotent re-grant can.
func (q *Queue) Retry(j *job.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !j.Snapshot().State.Outcome.IsSettled() {
		return errNotSettled
	}
	q.releaseFor(j, job.StateUnset)
	return j.BeginAttempt(q.now())
}

// Advance is the scheduling loop's entry point for one job. A blocked path
// never records a verdict — State, Next and Outcome are untouched — so a lost
// acquisition race costs a tick, never a decision. It is not true that a
// blocked path writes NO job state: branch 3's `if !q.grantFor(...) { return
// nil }` can block after grantFor has already written j.lease via j.Grant,
// which Snapshot().HoldsLease then reports, and grantFor's own comment
// documents that write as deliberate — the job keeps a lease it is one slot
// short of using, rather than giving up capacity it will need again on the
// next tick. Advance takes no target — the target is next, written by the
// worker that finished the state.
func (q *Queue) Advance(j *job.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	s := j.Snapshot()
	if s.Intent == job.IntentCancel {
		return q.finishCancel(j, s)
	}

	// 1. Never run: start it, if permitted. No lease is needed or taken here;
	//    branch 2 grants it, exactly as for a paused or restarted job.
	if s.State.State == job.StateUnset {
		if _, gated := q.gatedBy(s); gated {
			return nil
		}
		return j.BeginAttempt(q.now())
	}
	// A SETTLED attempt is never reopened here. Retry is an explicit user
	// action, not something a scheduling tick decides — q.Retry is the door.
	if s.State.Outcome.IsSettled() {
		// A settled attempt KEEPS the position it settled at (§3.3) but needs
		// none of that position's resources. Without this, a job that settles
		// in Assessing, Repairing, Extracting or Finalizing holds pool-B
		// capacity forever: no other path frees it, because every other
		// release is on a path a settled job never takes again.
		q.releaseFor(j, job.StateUnset)
		return nil
	}
	// 2. Current state's work is unfinished: make it runnable. This is the
	//    resume path AND the restart path — they are the same path.
	if s.State.Next == job.StateUnset {
		if q.holds(j.ID(), s) {
			return nil // already working; never take resources from a live worker
		}
		if _, gated := q.gatedBy(s); gated {
			return q.parkLocked(j)
		}
		q.grantFor(j, s.State.State)
		return nil
	}
	// 3. Work is finished: move.
	if _, gated := q.gatedBy(s); gated {
		return q.parkLocked(j)
	}
	if job.IsCorrectness(s.State.State) && job.IsProduction(s.State.Next) {
		l, err := j.Cross(s.State.Next)
		if err != nil {
			return err
		}
		if err := q.reclaim(l); err != nil {
			return err
		}
		// Deliberately ignoring the result: the decision was already recorded
		// in next, crossing only ADDS pool-A capacity, and a job that crosses
		// and then fails to get a slot is simply not running until it does.
		// Branch 2 grants it on a later tick. It cannot go back.
		q.grantFor(j, s.State.Next)
		return nil
	}
	if !q.grantFor(j, s.State.Next) {
		return nil
	}
	if err := j.Transition(s.State.Next); err != nil {
		// grantFor has no rollback (its own doc comment): it acquired what the
		// DESTINATION requires and does not know the move failed. The job is
		// still at its ORIGIN position — s.State.State, unchanged by a refused
		// Transition — so release what THAT position does not need, through the
		// same owner every other release goes through, rather than calling
		// q.slots.release directly. Without this, a slot grantFor just acquired
		// for a destination the job never reached stays in q.slots.held forever:
		// nothing else on this path frees it, because every other release call
		// is keyed to a state the job actually reached.
		q.releaseFor(j, s.State.State)
		return err
	}
	// A DEMOTION frees what the new position does not need. Assessing →
	// Fetching is the live case and the only one in the work spine: Assessing
	// holds a slot, Fetching is network-bound and holds none (§3.4), so
	// without this the job downloads for minutes or hours while occupying
	// pool B. Released AFTER the move, so a refused Transition cannot leave
	// the job resourceless at the position it is still occupying.
	q.releaseFor(j, s.State.Next)
	return nil
}
