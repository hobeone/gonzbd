package dispatch

import (
	"fmt"

	"github.com/hobeone/gonzbd/internal/job"
)

// Header is the display metadata a listing needs that internal/job.Job does
// not carry. job.Job holds id, name and policy only; category, priority and
// the total byte figure live in internal/queue until B2.4 migrates them, so
// the caller supplies them at Add.
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
}

// Add registers a job in queue order and wakes the tick.
//
// A duplicate ID is an error rather than an overwrite: the registry is the only
// route by which a job's resources are returned, so replacing an entry would
// strand whatever the displaced job held, with nothing left to release it.
func (d *Dispatcher) Add(j *job.Job, h Header) error {
	d.mu.Lock()
	if _, dup := d.byID[j.ID()]; dup {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: Add: job %q is already registered", j.ID())
	}
	d.byID[j.ID()] = &entry{j: j, h: h}
	d.order = append(d.order, j.ID())
	d.mu.Unlock()

	d.kick()
	return nil
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

// remove deletes a job from d.byID, d.order and d.written under d.mu.
//
// It preserves the relative order of the remaining entries: queue order is
// the priority policy sched consults, and a swap-with-last deletion would
// silently reorder jobs behind the removed one. evictCancelledNeverRun
// (tick.go) is the first caller — it never removes a running job, but this
// method makes no such assumption itself.
//
// d.written is pruned here too, not left to grow unboundedly: without this,
// a job removed from the registry keeps its last-Persisted entry forever,
// and if the store or a caller later reuses that same job ID, the reused
// job's first persistIfChanged would compare against the dead job's stale
// row and could wrongly suppress a Save that should have happened.
func (d *Dispatcher) remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.byID, id)
	delete(d.written, id)
	for i, oid := range d.order {
		if oid == id {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
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
