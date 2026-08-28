package sched

import (
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// Workers is what the Queue needs from the execution side. Half B2 implements
// it; B1 tests it with a stub. It is an interface rather than a concrete type
// so that this package's tests never need a real downloader, and so the
// dependency points from B2 to here rather than the other way.
type Workers interface {
	// Abort tells the worker running this job to stop. It returns
	// immediately; the job settles on a later tick, once running() has gone
	// false. §3.7: "immediately" describes when the worker is TOLD to stop,
	// not when its resources are taken.
	Abort(jobID string)
}

// Queue owns admission: the pure predicates over a job.Snapshot (holds,
// running, gatedBy, waitReason) and the two resource-return paths (reclaim,
// releaseFor) that Cancel needs.
//
// mu guards the pools and the admission decisions made over them. Cancel is
// its first locker (q.mu.Lock(); defer q.mu.Unlock()); prior spec §7.1's
// order is Queue.mu before Job.mu, so nothing here may call into a *job.Job
// method while holding mu in a way that would take the locks in the other
// order.
type Queue struct {
	mu     sync.Mutex
	paused bool
	leases *leasePool
	slots  *slotPool
	clock  func() time.Time
	work   Workers
}

// New builds a Queue with the given pool A and pool B capacities, an injected
// clock, and the Workers it aborts jobs through.
func New(leaseCap, slotCap int, clock func() time.Time, w Workers) *Queue {
	return &Queue{
		leases: newLeasePool(leaseCap),
		slots:  newSlotPool(slotCap),
		clock:  clock,
		work:   w,
	}
}

func (q *Queue) now() time.Time { return q.clock() }

// reclaim is the SOLE reclaimer (§3.9). It no-ops on nil so that no call site
// has to test for it — a job may legitimately reach the crossing holding
// nothing, having been paused at Assessing{next: Extracting} and resumed.
//
// It lives here rather than beside advance because Cancel needs it too, and
// Cancel lands first: a task whose code does not compile on its own is not a
// task, and every commit must leave the repository building.
func (q *Queue) reclaim(l *job.Lease) error { return q.leases.reclaim(l) }

// releaseFor is the SOLE owner of slot release, and grantFor's dual: it frees
// what s does not require. job.StateUnset requires nothing, which is how park,
// settlement and cancel free everything without naming pool B at the call.
//
// It lives here beside reclaim, rather than beside grantFor in advance.go, for
// exactly reclaim's reason: finishCancel needs it and Cancel lands first.
// Together the two are pool A's and pool B's single return paths.
//
// It exists because a review of this plan found THREE separate slot leaks —
// the Assessing → Fetching demotion, park, and settlement — where a per-site
// release would have had to be remembered at each, and was not. The asymmetry
// was total and is the whole argument: the lease got an owner in Task 4 and
// leaked nowhere; the slot had none and leaked everywhere.
//
// The asymmetry was not acquire-count versus release-count. Acquisition was
// designed to have an owner from the start — grantFor is meant to be the sole
// production caller of slots.acquire, but grantFor is Task 7 work and does
// not exist at this commit: `grep -n 'slots\.acquire' internal/sched/*.go |
// grep -v _test.go` finds only this comment's own mention of the name, zero
// production call sites. Release, by contrast, never had a designated owner
// at all — one ad-hoc call inside finishCancel — which is why the three paths
// that never reach finishCancel each leaked.
//
// Slots differ from leases in that nothing travels with them: the job does not
// carry one, so there is no Surrender to mirror, and release is idempotent
// (slotPool.release is a map delete). That is why this takes a *job.Job and a
// target state rather than a token.
func (q *Queue) releaseFor(j *job.Job, s job.State) {
	if !needsSlot(s) {
		q.slots.release(j.ID())
	}
}

// holds reports whether the job holds everything its CURRENT position
// requires. It says nothing about whether the attempt is open — running()
// adds that, and §3.4 explains why the two must stay separate.
func (q *Queue) holds(id string, s job.Snapshot) bool {
	pos := s.State.State
	if needsLease(pos) && !s.HoldsLease {
		return false
	}
	if needsSlot(pos) && !q.slots.holds(id) {
		return false
	}
	return true
}

// running is §3.4's three-conjunct definition. Every conjunct is load-bearing:
// a job whose work has finished is waiting to move, not running (the next
// clause); and a settled attempt keeps its position, so holds() may be
// genuinely true for it — only the open clause excludes it.
func (q *Queue) running(id string, s job.Snapshot) bool {
	return s.IsOpen() && q.holds(id, s) && s.State.Next == job.StateUnset
}

// gatedBy reports an intent or queue-wide gate. Resources are NOT consulted:
// they are a grant question, not a gate question.
//
// IntentCancel is absent deliberately — advance handles it first, so no cancel
// value reaches the render path.
func (q *Queue) gatedBy(s job.Snapshot) (job.WaitReason, bool) {
	if s.Intent == job.IntentPause {
		return job.UserPaused, true
	}
	if q.paused {
		return job.GlobalPause, true
	}
	return 0, false
}

// waitReason explains why this job is not running, about its CURRENT state.
func (q *Queue) waitReason(id string, s job.Snapshot) (job.WaitReason, bool) {
	if s.State.Outcome.IsSettled() {
		return 0, false // terminal: waiting for nothing, whatever its position requires
	}
	if q.running(id, s) {
		return 0, false
	}
	if r, gated := q.gatedBy(s); gated {
		return r, true
	}
	if s.State.State == job.StateUnset {
		return job.NoLease, true // waiting to start
	}
	want := s.State.State
	if s.State.Next != job.StateUnset {
		want = s.State.Next // work ended; it waits on what the NEXT state needs
	}
	if needsLease(want) && !s.HoldsLease {
		return job.NoLease, true
	}
	// This tail departs from the task brief's draft, which returned
	// NoComputeSlot unconditionally once the lease check passed:
	//
	//   if needsLease(want) && !s.HoldsLease {
	//       return job.NoLease, true
	//   }
	//   return job.NoComputeSlot, true
	//
	// That reports a job at Assessing{Next: Fetching} holding its lease as
	// perpetually slot-starved, because Fetching needs no slot at all — it
	// never checked needsSlot(want) before defaulting to NoComputeSlot.
	// TestWaitReason_UsesTheNextStateWhenWorkHasEnded pins the corrected
	// behaviour: work has ended and the job holds everything the next
	// position requires, so it is ready to be advanced, not waiting on
	// anything.
	if needsSlot(want) && !q.slots.holds(id) {
		return job.NoComputeSlot, true
	}
	return 0, false
}
