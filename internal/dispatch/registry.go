package dispatch

import (
	"fmt"
	"slices"

	"github.com/hobeone/gonzbd/internal/job"
)

// Header is the display metadata a listing needs. job.Job holds id, name and
// policy only; category, priority and the total byte figure live in
// internal/queue until B2.4 migrates them, so the caller supplies them at Add.
//
// Name is the one field job.Job DOES carry, and it is duplicated here on
// purpose: it lets a listing be composed from Header alone, without the
// registry handing out *job.Job pointers to do it. The duplication is a
// second copy of a display string, not a second source of truth for any
// scheduling decision — nothing reads Header.Name to decide anything.
type Header struct {
	Name     string
	Category string
	Priority int
	Bytes    int64
}

// Row is one line of a queue listing: the scheduling view sched computes,
// beside the header the caller supplied.
type Row struct {
	ID     string
	Header Header
	View   job.RenderView
}

// entry is the registry's record for one job.
type entry struct {
	j *job.Job
	h Header
	// seq is this job's place in queue order, persisted as Persisted.SortKey.
	// It is a monotonic insertion sequence rather than an index: a position
	// would have to be renumbered whenever an earlier job is removed, and
	// persistIfChanged only writes jobs whose state moved, so the rewrite
	// would be both expensive and easy to miss.
	//
	// A sequence needs no renumbering because only two operations change
	// d.order — register appends, and remove deletes with slices.Delete,
	// which preserves the relative order of what survives.
	// TestSortKey_ReproducesQueueOrderAcrossRemoval pins that.
	seq int64
}

// Add registers a job at the end of the queue and wakes the tick.
//
// A duplicate ID is an error rather than an overwrite: the registry is the only
// route by which a job's resources are returned, so replacing an entry would
// strand whatever the displaced job held, with nothing left to release it.
func (d *Dispatcher) Add(j *job.Job, h Header) error {
	d.mu.Lock()
	seq := d.nextSeq
	d.mu.Unlock()
	return d.register(j, h, seq)
}

// register is the only path by which a job enters the registry, and the only
// writer of d.byID, d.order and d.nextSeq. Add calls it with the next unused
// sequence; restore calls it with the sequence the store recorded, which is
// what preserves queue order across a restart.
//
// One path rather than two because the alternative had already produced a
// defect in review: a restore that registered through Add was handed a FRESH
// sequence while d.written held the row's real SortKey from disk, so the first
// persistIfChanged saw a difference that was not there and rewrote every
// restored row's key. Two registration paths that must agree about one field
// is the smell Standing Design Rule 2 names, and an owner is the fix it
// prescribes.
//
// Advancing d.nextSeq past seq here, rather than at the call sites, is what
// makes "the next Add sorts after everything restored" true without a separate
// step anyone could forget.
func (d *Dispatcher) register(j *job.Job, h Header, seq int64) error {
	d.mu.Lock()
	if _, dup := d.byID[j.ID()]; dup {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: register: job %q is already registered", j.ID())
	}
	d.byID[j.ID()] = &entry{j: j, h: h, seq: seq}
	d.order = append(d.order, j.ID())
	d.nextSeq = max(d.nextSeq, seq+1)
	d.mu.Unlock()

	d.kick()
	return nil
}

// sortKeyOf reports a registered job's queue-order sequence, or -1 if it is not
// registered. Tests use it to assert on the key itself: asserting on List order
// instead would prove nothing about the sequence, since register appends
// unconditionally and a new job is therefore last either way.
func (d *Dispatcher) sortKeyOf(id string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return -1
	}
	return e.seq
}

// snapshotOrder copies the registry in queue order. The copy exists so the tick
// can release d.mu before calling into sched: D-B9 forbids holding d.mu across
// such a call, because Workers.Abort runs inside Queue.mu and an Abort that
// took d.mu would deadlock ABBA against a concurrent Cancel.
func (d *Dispatcher) snapshotOrder() []*job.Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*job.Job, 0, len(d.order))
	for _, id := range d.order {
		out = append(out, d.byID[id].j)
	}
	return out
}

// remove deletes a job from EVERY per-job structure — d.byID, d.order,
// d.written, d.resident and d.launched — under one d.mu span. See the
// Dispatcher struct's per-job map comment (dispatch.go) for why "every" is
// the rule here rather than "the ones this caller happens to care about".
//
// It preserves the relative order of the remaining entries: queue order is
// the priority policy sched consults, and a swap-with-last deletion would
// silently reorder jobs behind the removed one. evictCancelledNeverRun
// (tick.go) is the first caller — it never removes a running job, but this
// method makes no such assumption itself.
//
// What each prune buys, since none of them is obviously load-bearing on its
// own and all three were omitted at least once:
//
//   - d.written: a job removed from the registry would keep its last-Persisted
//     entry forever, and a reused job ID's first persistIfChanged would
//     compare against the dead job's stale row and wrongly suppress a Save.
//   - d.resident: a stale true entry makes reconcileResidency take neither
//     branch (its hydrate arm requires !d.isResident(id)), so a reused ID
//     never hydrates and runs without its manifest.
//   - d.launched: a stale true entry makes claimLaunched return false
//     forever, so a reused ID is permanently unlaunchable.
func (d *Dispatcher) remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.byID, id)
	delete(d.written, id)
	delete(d.resident, id)
	delete(d.launched, id)
	// slices.Delete rather than the append-shift idiom: the shift leaves the
	// vacated tail slot holding its old string header, and d.order lives as
	// long as the Dispatcher, so a removed job's ID stayed reachable from the
	// backing array. slices.Delete zeroes what it vacates.
	if i := slices.Index(d.order, id); i >= 0 {
		d.order = slices.Delete(d.order, i, i+1)
	}
}

// List composes the queue listing. It takes Queue.mu exactly once, through
// RenderAll, so every row is from one instant.
func (d *Dispatcher) List() []Row {
	d.mu.Lock()
	ids := make([]string, len(d.order))
	copy(ids, d.order)
	js := make([]*job.Job, 0, len(ids))
	hs := make([]Header, 0, len(ids))
	for _, id := range ids {
		e := d.byID[id]
		js = append(js, e.j)
		hs = append(hs, e.h)
	}
	d.mu.Unlock()

	views := d.q.RenderAll(js)
	out := make([]Row, 0, len(ids))
	for i, id := range ids {
		out = append(out, Row{ID: id, Header: hs[i], View: views[i]})
	}
	return out
}
