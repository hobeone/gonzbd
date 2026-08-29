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
// Eviction of a cancelled never-run job (D-B12) runs after Advance and before
// reconcileResidency: Advance routes IntentCancel to finishCancel before every
// other branch, so eviction cannot race a settle. This tick advances, evicts,
// reconciles residency, then launches.
func (d *Dispatcher) tick(ctx context.Context) {
	for _, j := range d.snapshotOrder() {
		if err := d.q.Advance(j); err != nil {
			d.logAdvanceError(j.ID(), err)
			continue
		}
		if d.evictCancelledNeverRun(ctx, j) {
			continue
		}
		if err := d.reconcileResidency(ctx, j); err != nil {
			d.logResidencyError(j.ID(), err)
			continue
		}
		d.launch(ctx, j)
	}
}

// evictCancelledNeverRun removes a job the user cancelled before it ever ran,
// and reports whether it did.
//
// finishCancel (internal/sched/cancel.go) returns nil for such a job because
// Outcome lives on the Attempt and there is none, so Finish would return
// ErrNoOpenAttempt. The job therefore survives in the Queue: gatedBy ignores
// IntentCancel, waitReason returns NoLease, NoLease.IsPause() is false, and
// job.ToSABnzbd renders StatusQueued. A job the user deleted looks queued,
// forever — the dispatcher is the only component with a registry to remove it
// from, which is why the eviction lives here rather than in sched.
//
// It frees no pools: TestRequirements_StateUnsetRequiresNothing
// (internal/sched/requirements_test.go) pins that StateUnset requires neither
// a lease nor a slot, so there is nothing for this function to release before
// removing the job.
func (d *Dispatcher) evictCancelledNeverRun(ctx context.Context, j *job.Job) bool {
	v := d.q.Render(j)
	if v.State != job.StateUnset || v.Intent != job.IntentCancel {
		return false
	}
	if err := d.store.Delete(ctx, j.ID()); err != nil {
		// Leave it registered: removing it from the registry here while the
		// store still holds the row would resurrect it at the next Start,
		// which is worse than trying again on the next tick.
		d.logStoreError(j.ID(), err)
		return false
	}
	d.remove(j.ID())
	return true
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
