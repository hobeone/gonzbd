package sched

import "github.com/hobeone/gonzbd/internal/job"

// park releases what a gated job must not keep holding. Both gated paths go
// through it.
//
// It returns an error where spec §3.6 shows it returning nothing. That is a
// deliberate divergence and it SUPERSEDES a specific spec decision: §10's
// revision history records "park returned an error that is always nil →
// returns nothing", so the signature was narrowed once, on the grounds that
// the error could not carry information. That reasoning no longer holds. Task 4
// gave reclaim an identity audit, so it now fails on a lease this pool did not
// issue or already took back — a real condition, and one whose only other
// outlet would be silence.
func (q *Queue) park(j *job.Job) error {
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
			// `grep -n 'q\.grantFor(' advance.go` finds exactly three —
			// branch 2 (line 133) and branch 3's two calls (lines 152, 155).
			// At all three, Advance's settled early return has already
			// established an open, unsettled attempt before grantFor runs,
			// so "no open attempt" and "past the boundary" cannot fire from
			// any of them; and at line 152, needsLease(s) is false for both
			// Production states, so this whole block is never entered there
			// at all. That leaves the fifth as the only refusal a production
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
func (q *Queue) Retry(j *job.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.releaseFor(j, job.StateUnset)
	return j.BeginAttempt(q.now())
}

// Advance is the scheduling loop's entry point for one job. It writes no job
// state on any blocked path, so a lost acquisition race costs a tick, never a
// verdict. It takes no target — the target is next, written by the worker that
// finished the state.
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
			return q.park(j)
		}
		q.grantFor(j, s.State.State)
		return nil
	}
	// 3. Work is finished: move.
	if _, gated := q.gatedBy(s); gated {
		return q.park(j)
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
