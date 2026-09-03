package sched

import (
	"errors"
	"fmt"

	"github.com/hobeone/gonzbd/internal/job"
)

// ErrNotOutstanding is returned by leasePool.reclaim for a lease this pool did
// not issue, or already got back. Both are caller bugs that inflate capacity:
// a double return frees the same lease's pool-A capacity twice, so two jobs
// end up holding what the pool's own accounting believes is a single lease.
// Exported so a caller of Park or Settle can errors.Is against it.
var ErrNotOutstanding = errors.New("sched: lease is not outstanding")

// leasePool issues pool-A admission tokens and audits their return.
//
// It tracks ids rather than pointers because it must be able to say "I did not
// issue this", which pointer identity alone cannot express — and because a
// pointer-keyed map was outright broken while job.Lease was zero-sized: a
// probe on PR #447 measured unsafe.Sizeof(Lease{}) == 0, so distinct
// allocations shared one address and two jobs' leases collapsed into one map
// entry. internal/job/lease.go's LeaseID and NewLease are what fixed it, by
// giving each issuance an identity independent of the pointer.
//
// The audit exists because nothing else can enforce it. job.Cross and
// job.Finish return a *Lease and Go has no must-use; a caller writing
// `_, err := j.Cross(to)` silently drops a lease, and neither go vet nor
// golangci-lint sees it. Slots are not returned from either door — they are
// keyed by string job ID in slotPool, not carried on the job the way a lease
// is — so nothing here is ever silently dropped on the pool-B side; this
// audit is pool A's alone. The pool knowing what is outstanding is what lets
// a scenario test assert none were lost (spec §6, test 4b).
//
// The audit's reach is bounded to what one leasePool itself issued: `next`
// starts at zero independently in every leasePool, so a second Queue's pool
// would mint LeaseID(1) too, and this pool's issued map would accept that
// foreign lease as its own if handed one — id collision, not identity
// confusion, and only across two *different* Queues in the same process.
//
// A second Queue is no longer only a hypothetical: internal/dispatch.New
// (internal/dispatch/dispatch.go) mints one *Queue per *Dispatcher —
// `grep -n 'sched.New(' internal/dispatch/dispatch.go` finds one call, inside
// New — so constructing two Dispatchers does construct two Queues, and
// internal/dispatch's own TestTick_LogsAndSkipsAJobWhoseAdvanceErrors
// (tick_test.go) deliberately does this: it hands the same *job.Job to two
// Dispatchers so the second one's queue, which never issued that job's
// lease, hits this exact audit and fails as designed. That test is evidence
// the audit does its job across two Queues, not evidence two Queues sharing
// a Job is a supported configuration — nothing in dispatch's own design
// (D-B13, this Dispatcher owns its Queue outright) constructs more than one
// Dispatcher per Job, and production code constructs exactly one Dispatcher per
// Application: `grep -rln 'dispatch\.New(' --include='*.go' internal/ cmd/ | grep -v _test.go`
// (run from the repo root) finds 1 file (internal/app/app.go). So
// today's single-pool claim survives, scoped to what runs: within a single
// pool, which is every production configuration that exists today, identity
// is exact — a double return or a lease this pool never issued is caught,
// which is what reclaim's ErrNotOutstanding and the tests below rely on.
// Whether a future caller is allowed to construct two Queues over one Job is
// a design question for whoever adds that caller, not settled by this
// comment.
//
// Not goroutine-safe: every caller is expected to hold Queue.mu. Stated
// rather than locked, because a second lock here would be a second thing to
// order against Queue.mu and Job.mu (prior spec §7.1). Queue.mu exists and is
// taken by ten production doors — Cancel (cancel.go), Park, Retry and
// Advance (advance.go), Settle (settle.go), Render and RenderAll (render.go),
// and Pause, Resume and Paused (queue.go; see queue.go's own comment on mu) —
// so this is now a description of a lock real code takes, not a
// forward-looking constraint. queue.go's own comment carries the backticked
// grep for the count; internal/sched.TestQueueMuLockers_MatchTheEnumerationStatedInProse
// is what checks that these ten NAMES, not merely this count, are still
// right — a grep proves how many, never which ones.
type leasePool struct {
	capacity int
	next     job.LeaseID
	issued   map[job.LeaseID]bool
}

func newLeasePool(capacity int) *leasePool {
	return &leasePool{capacity: capacity, issued: make(map[job.LeaseID]bool, capacity)}
}

// issue mints a lease, or returns nil when the pool is at capacity. nil means
// "no capacity", which callers already handle — grantFor returns false and the
// job waits with reason NoLease.
func (p *leasePool) issue() *job.Lease {
	if len(p.issued) >= p.capacity {
		return nil
	}
	p.next++
	p.issued[p.next] = true
	return job.NewLease(p.next)
}

// reclaim returns a lease to the pool. It no-ops on nil, which §3.9 requires:
// a job may legitimately reach the crossing holding nothing, and one function
// that accepts nil is fewer chances to forget than a nil check at every exit.
func (p *leasePool) reclaim(l *job.Lease) error {
	if l == nil {
		return nil
	}
	if !p.issued[l.ID()] {
		return fmt.Errorf("%w: id %d", ErrNotOutstanding, l.ID())
	}
	delete(p.issued, l.ID())
	return nil
}

func (p *leasePool) outstanding() int { return len(p.issued) }

// slotPool is pool-B compute capacity, held by job ID. Slots have no object
// because nothing travels with them — unlike a lease, which carries the
// Manifest and StorageBarrier (spec §6).
type slotPool struct {
	capacity int
	held     map[string]bool
}

func newSlotPool(capacity int) *slotPool {
	return &slotPool{capacity: capacity, held: make(map[string]bool, capacity)}
}

// acquire is idempotent: a job that already holds a slot keeps it and the call
// reports success, so a caller re-granting for a state it is already running
// does not consume a second slot.
func (p *slotPool) acquire(id string) bool {
	if p.held[id] {
		return true
	}
	if len(p.held) >= p.capacity {
		return false
	}
	p.held[id] = true
	return true
}

func (p *slotPool) release(id string)    { delete(p.held, id) }
func (p *slotPool) holds(id string) bool { return p.held[id] }
func (p *slotPool) outstanding() int     { return len(p.held) }
