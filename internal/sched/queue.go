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
	//
	// Abort MUST NOT block, and MUST NOT acquire any lock that a caller could
	// hold across a call into Queue. cancel.go calls Abort from inside
	// Queue.mu's span — it is the only outward call this package makes while
	// holding that lock, and check_lock_io cannot see through an interface
	// method implemented in another package. If a B2 dispatcher takes its own
	// lock on a tick and calls Queue.Advance (which takes Queue.mu), an Abort
	// implementation that takes that same dispatcher lock deadlocks ABBA
	// against a concurrent Cancel. This is a precondition B2's implementation
	// of Workers must satisfy; nothing in this package enforces it.
	Abort(jobID string)
}

// Queue owns admission: the pure predicates over a job.Snapshot (holds,
// running, gatedBy, waitReason) and the two resource-return paths (reclaim,
// releaseFor) that Cancel needs.
//
// mu guards the pools and the admission decisions made over them. Cancel was
// its first locker during this package's build order (queue.go landed before
// advance.go so that reclaim and releaseFor — which finishCancel needs —
// existed for Cancel to call), but it is no longer the only one: `grep -n
// 'q\.mu\.Lock' internal/sched/*.go | grep -v _test.go` finds four production
// lockers — Cancel (cancel.go), and park, Retry and Advance (advance.go) —
// plus this sentence's own mention of the pattern, written as q.mu.Loc[k]()
// (the same self-matching workaround internal/job/job.go's surrenderLocked
// comment uses) so it does not inflate that count. Prior spec §7.1's order is
// Queue.mu before Job.mu, so nothing here may call into a *job.Job method
// while holding mu in a way that would take the locks in the other order.
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
//
// w must not be nil: cancel.go's interrupt arm dereferences it unconditionally
// (q.work.Abort), and a nil Workers is a construction-time programmer error —
// not state an earlier build wrote, so Standing Design Rule 1's guard-removal
// argument does not apply here. New panics immediately rather than let a nil
// Workers surface as a nil-pointer dereference deep inside a later Cancel
// call, on an unrelated goroutine, with no construction-site stack frame left
// to explain it.
func New(leaseCap, slotCap int, clock func() time.Time, w Workers) *Queue {
	if w == nil {
		panic("sched: New: Workers must not be nil")
	}
	if clock == nil {
		// Same class as the nil Workers above: a construction-time programmer
		// error, not state an earlier build wrote, so Rule 1 does not waive it.
		// Without this, New succeeds and the first Advance, Cancel or Retry
		// panics inside q.now() — far from the call that was actually wrong.
		panic("sched: New: clock must not be nil")
	}
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
// designed to have an owner from the start — grantFor (advance.go, Task 7) IS
// the sole production caller of slots.acquire: `grep -n 'slots\.acquire'
// internal/sched/*.go | grep -v _test.go` now finds exactly two lines, the
// call inside grantFor and this comment's own mention of the name — one
// production call site. Release, by contrast, never had a designated owner
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
// a job whose work has finished is waiting to move, not running (the
// Next == StateUnset clause); and a settled attempt keeps its position, so
// holds() may be genuinely true for it — only the open clause excludes it.
//
// The order is deliberate, though the value is order-independent: IsOpen and
// the Next comparison are inline scalar checks, while holds() consults the
// requirements table and does a map lookup in slotPool. Testing the two cheap
// conjuncts first means a job awaiting a transition — the common case for the
// whole time work has ended and the move has not happened — never pays for the
// lookup.
func (q *Queue) running(id string, s job.Snapshot) bool {
	return s.IsOpen() && s.State.Next == job.StateUnset && q.holds(id, s)
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
