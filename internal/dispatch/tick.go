package dispatch

import (
	"context"
	"errors"
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
// reconciles residency, launches, then persists.
//
// Persistence has two exits, not one. The linear order above is the path a
// healthy job takes; a job whose residency reconciliation fails also persists,
// because that failure can have SETTLED it. Only two exits skip the write: an
// Advance error, which produces no position this Queue accepted, and an
// eviction, which has already deleted the row.
func (d *Dispatcher) tick(ctx context.Context) {
	for _, j := range d.snapshotOrder() {
		if err := d.q.Advance(j); err != nil {
			d.logAdvanceError(j.ID(), err)
			continue
		}
		// True means this job's pass ends here, including when the store
		// delete failed and the job stays registered for a retry.
		if d.evictCancelledNeverRun(ctx, j) {
			continue
		}
		if err := d.reconcileResidency(ctx, j); err != nil {
			d.logResidencyError(j.ID(), err)
			// reconcileResidency may have SETTLED the job before returning
			// this error: an unreadable manifest is a fact about the job, so
			// that branch settles it Failed and returns its pools. The settle
			// is a real state change and has to reach the store on this pass.
			// A later tick would persist it, but Stop is what makes the gap
			// permanent — it ends the loop, and the row still says Pending
			// for a job that can never run, which the next Start restores as
			// pending work.
			//
			// The hydration-cancelled sub-case costs nothing here: it does
			// not settle, so the rendered value equals the one already
			// written and persistIfChanged returns before calling Save.
			//
			// Advance's branch above deliberately does NOT do this. A
			// position Advance reached and then failed on is not one this
			// Queue accepted, so the store keeps the last one it did.
			d.persistIfChanged(ctx, j)
			continue
		}
		d.launch(ctx, j)
		d.persistIfChanged(ctx, j)
	}
}

// persistIfChanged writes a job's four axes when they have moved since the
// last write. The comparison is against the last value written, held in
// memory (d.written), so a quiet tick over a large queue costs no store
// traffic — Persisted is a plain comparable struct (no slices or maps), so
// `last == p` is a real equality check, not a reference comparison.
//
// D-B9: d.mu is never held across the Snapshot or Save calls. lastWritten and
// markWritten each take d.mu for one map operation and release it
// immediately; Snapshot (into job, taking Job.mu) and Save (into Store) both
// run unlocked between them. This read was d.q.Render until the two facts
// Persisted records — the job's own StateView and Intent — were taken
// straight from Snapshot instead, which drops a Queue.mu acquisition that
// bought this function nothing.
func (d *Dispatcher) persistIfChanged(ctx context.Context, j *job.Job) {
	h, seq, ok := d.entryFor(j.ID())
	if !ok {
		// Evicted (D-B12) or removed between snapshotOrder and here: nothing
		// left in the registry to attach a Header to.
		return
	}
	// Snapshot for the same reason as evictCancelledNeverRun: Persisted
	// carries only the job's own StateView and Intent, so Render's Queue.mu
	// acquisition would buy nothing this row records.
	s := j.Snapshot()
	p := Persisted{
		ID:      j.ID(),
		SortKey: seq,
		Header:  h,
		Policy:  j.Policy(),
		State:   s.State,
		Intent:  s.Intent,
	}
	if ds := j.DownloadStarted(); !ds.IsZero() {
		p.DownloadStarted = ds.Unix()
	}
	if df := j.DownloadFinished(); !df.IsZero() {
		p.DownloadFinished = df.Unix()
	}
	p.Par2ReleaseReason = j.Par2ReleaseReason()
	p.RecoveryBytes = j.RecoveryBytes()
	if last, ok := d.lastWritten(j.ID()); ok && last == p {
		return
	}
	d.storeMu.Lock()
	d.mu.Lock()
	if d.removing[j.ID()] || d.byID[j.ID()] == nil {
		d.mu.Unlock()
		d.storeMu.Unlock()
		return
	}
	d.mu.Unlock()

	if err := d.store.Save(ctx, p); err != nil { //lockio: storeMu serializes store.Save with store.Delete to prevent row resurrection
		d.storeMu.Unlock()
		d.logStoreError(j.ID(), err)
		return
	}
	d.storeMu.Unlock()
	d.markWritten(p)
}

// evictCancelledNeverRun removes a job the user cancelled before it ever ran,
// and reports whether THIS JOB'S PASS IS OVER — which is not the same as
// whether the removal succeeded, and the difference was a defect.
//
// The bool used to mean "did I evict?", so a failed store.Delete returned
// false and tick could not tell that case apart from "this is not a cancelled
// never-run job at all". It walked on to persistIfChanged, and because the
// Intent had just become IntentCancel the rendered value no longer matched
// lastWritten, so Save wrote the row straight back — the same tick re-created
// the row whose deletion had just failed.
//
// A cancelled never-run job needs nothing else from this tick either way: it
// holds no resources to reconcile, there is nothing to launch, and its row
// must not be rewritten. So the answer is true whether the delete succeeded
// or not, and the retry rides on the job still being registered.
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
	// j.Snapshot, not d.q.Render: the two fields this reads are the JOB's
	// own, and Snapshot takes them under one Job.mu acquisition. Render would
	// additionally take Queue.mu to compute Running, Reason and Holds, none
	// of which this function consults. Nothing is weakened by the narrower
	// read — Snapshot is atomic for what it returns, so the pair cannot be
	// observed torn, which is the only property the decision below needs.
	s := j.Snapshot()
	if s.State.State != job.StateUnset || s.Intent != job.IntentCancel {
		return false
	}
	d.storeMu.Lock()
	err := d.store.Delete(ctx, j.ID())
	d.storeMu.Unlock()
	if err != nil {
		// Leave it registered: removing it from the registry here while the
		// store still holds the row would resurrect it at the next Start,
		// which is worse than trying again on the next tick. Returning true
		// anyway is what keeps the rest of the tick — above all
		// persistIfChanged — from writing the row back.
		d.logStoreError(j.ID(), err)
		return true
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
// function, and a read that failed on the manifest itself settles the job
// here. A read that failed because ctx was cancelled does NOT settle — see the
// branch below for why the two differ.
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
			// A cancelled context says nothing about the JOB. run returns on
			// ctx.Done(), but a tick already in flight walks on with the same
			// cancelled ctx, so Hydrate reports context.Canceled for a
			// perfectly healthy job — and Outcome is write-once, so settling
			// here would mark it Failed permanently and the user would find
			// it Failed after a restart when it was merely interrupted.
			//
			// That is exactly what makes this case different from the one
			// below. An unreadable manifest is a fact about the job: it can
			// never run, so settling is right and returns its pools. A
			// cancellation is a fact about the process. The job keeps its
			// resources; Stop's sweep parks them, and if the cancellation was
			// not a shutdown the next tick retries the hydration.
			// ctx.Err() is checked FIRST and matters most. The sentinel
			// tests below identify a cancellation only when the error
			// CARRIES the sentinel, and the I/O a real Hydrate does mostly
			// does not: os.Open returns *os.PathError, gzip and json return
			// io.ErrUnexpectedEOF, and a reader interrupted mid-file reports
			// a short read. None of those wrap a context error, so on the
			// sentinel test alone every one of them settled the job Failed
			// during a shutdown. Outcome is write-once, so that is permanent:
			// the user restarts to find healthy jobs marked Failed because
			// the process happened to be stopping while their manifests were
			// read. If ctx is already cancelled, no error identity should be
			// able to produce a settle.
			//
			// The sentinel tests are kept rather than replaced: a Hydrate
			// given a ctx with its OWN deadline can report DeadlineExceeded
			// while this ctx is still live.
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("hydrate %s: %w", j.ID(), err)
			}
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
	if d.removing[id] || d.byID[id] == nil {
		return
	}
	d.resident[id] = true
}

func (d *Dispatcher) markNotResident(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.resident, id)
}
