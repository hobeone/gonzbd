package dispatch

import (
	"context"
	"fmt"

	"github.com/hobeone/gonzbd/internal/job"
)

// tick is one pass over the registry. It is called only from run, so it never
// overlaps itself and needs no locking against another tick.
//
// It copies the registry under d.mu and releases it before the first Advance:
// D-B9 forbids holding d.mu across such a call, because Workers.Abort runs
// inside Queue.mu and an Abort implementation that took d.mu would deadlock
// ABBA against a concurrent Cancel.
//
// Worker launch and eviction-on-cancel are not implemented yet — they land in
// Tasks 5-6. This tick advances, then reconciles residency.
func (d *Dispatcher) tick(ctx context.Context) {
	for _, j := range d.snapshotOrder() {
		if err := d.q.Advance(j); err != nil {
			d.logAdvanceError(j.ID(), err)
			continue
		}
		if err := d.reconcileResidency(ctx, j); err != nil {
			d.logResidencyError(j.ID(), err)
		}
	}
}

// reconcileResidency brings a job's manifest residency in line with what it
// holds (D-B8: manifestResident(j) <=> q.holds(j)).
//
// The invariant is stated at TICK BOUNDARIES, not instantaneously. grantFor
// runs inside Advance under Queue.mu, so a job acquires resources before this
// function can hydrate it, and Hydrate does disk I/O that must not run under
// any lock. A job therefore holds a lease with no manifest for the length of
// one read. Nothing consumes that window: Task 5's launch path runs after this
// function, and a failed read settles the job here.
//
// It takes exactly ONE d.q.Render(j) call and reads v.Holds — job.RenderView's
// field computed by sched's renderLocked from q.holds(j.ID(), s)
// (internal/sched/render.go), which is "has every resource the job's current
// position requires", not merely "has a lease". A job at Extracting holds a
// compute slot and no lease, so j.HoldsLease() alone would under-report it as
// not holding; a second, separate Render call would risk two acquisitions of
// Queue.mu straddling a transition and observing a view true at no instant.
func (d *Dispatcher) reconcileResidency(ctx context.Context, j *job.Job) error {
	v := d.q.Render(j)
	switch {
	case v.Holds && !d.isResident(j.ID()):
		if err := d.res.Hydrate(ctx, j.ID()); err != nil {
			// The job cannot run without its manifest, and it is holding
			// resources it can never use. Settling returns both pools; leaving
			// it would strand them, because no later tick reaches a different
			// branch for it.
			if serr := d.q.Settle(j, job.OutcomeFailed); serr != nil {
				return fmt.Errorf("hydrate %s: %w (and settle failed: %w)", j.ID(), err, serr)
			}
			d.markNotResident(j.ID())
			return fmt.Errorf("hydrate %s: %w", j.ID(), err)
		}
		d.markResident(j.ID())
	case !v.Holds && d.isResident(j.ID()):
		d.res.Evict(j.ID())
		d.markNotResident(j.ID())
	}
	return nil
}

// isResident, markResident and markNotResident are the dispatcher's own
// bookkeeping for what Residency believes is loaded. Each takes d.mu around a
// single map operation and releases it immediately — never across the
// Hydrate/Evict calls in reconcileResidency, which do disk I/O in production
// and must not run under any lock (D-B9).
func (d *Dispatcher) isResident(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.resident[id]
}

func (d *Dispatcher) markResident(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resident[id] = true
}

func (d *Dispatcher) markNotResident(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.resident, id)
}
