package dispatch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
)

// ErrNotFound is returned when an operation names a job ID that is not registered.
var ErrNotFound = errors.New("dispatch: job not found")

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
	Name      string
	Filename  string
	Category  string
	Priority  int
	Bytes     int64
	Warning   string
	Script    string
	Password  string
	PP        int
	NZBBackup string
	URL       string
	MD5       string
	Added     int64
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
	// It is an insertion sequence rather than an index: a position would have
	// to be renumbered whenever an earlier job is removed, and
	// persistIfChanged only writes jobs whose state moved, so the rewrite
	// would be both expensive and easy to miss.
	//
	// Nothing revises it TODAY, and that is a statement about the operations
	// that exist rather than a property of the design. The two that change
	// d.order — register's append and remove's order-preserving slices.Delete
	// — both leave surviving keys alone, so the sequence keeps reproducing
	// queue order without a renumbering pass.
	// TestSortKey_ReproducesQueueOrderAcrossRemoval pins that.
	//
	// It will not stay true. Spec §4.7 and /api?mode=switch require arbitrary
	// reordering, and a reorder has to renumber: restore rebuilds order from
	// SortKey alone, so a move that did not rewrite keys would be discarded at
	// the next restart. B2.4 owns that, because it is the change that gives
	// reorder a caller, and it needs a whole-queue resequence that is atomic
	// in the store rather than a per-job Save — a partial rewrite leaves the
	// queue in an order no user asked for. Recorded on #454.
	seq int64
}

// Add registers a job at the end of the queue and wakes the tick.
//
// A duplicate ID is an error rather than an overwrite: the registry is the only
// route by which a job's resources are returned, so replacing an entry would
// strand whatever the displaced job held, with nothing left to release it.
func (d *Dispatcher) Add(j *job.Job, h Header) error {
	// The two refusals live here rather than in register, because register is
	// also restore's door and restore runs during exactly the window the
	// second one closes.
	d.mu.Lock()
	switch {
	case d.stopped:
		// Registering here would strand the job: it would take a sequence,
		// enter d.byID and d.order, and prime d.wake with no loop left to read
		// it. The caller would get nil and the job would never tick, persist or
		// run — a failure nothing reports.
		d.mu.Unlock()
		return errors.New("dispatch: Add: this Dispatcher is stopped")
	case d.restoring:
		// A job interleaved into a half-rebuilt registry takes a sequence from
		// the counter before restore has raised it past the stored keys, so it
		// collides with a row still to be registered. It also survives a failed
		// restore: the rollback removes only the IDs restore itself registered.
		d.mu.Unlock()
		return errors.New("dispatch: Add: this Dispatcher is restoring; retry once Start returns")
	}
	d.mu.Unlock()

	if h.Added == 0 {
		h.Added = time.Now().UTC().Unix()
	}
	j.SetAdded(time.Unix(h.Added, 0).UTC())

	return d.register(j, h, seqNext)
}

// seqNext tells register to allocate the next unused sequence itself. It is a
// sentinel rather than Add reading d.nextSeq and passing the value, because
// reading it in one lock span and using it in another lets two concurrent Adds
// allocate the SAME key. Nothing fails at the time — both jobs register, both
// persist, the queue looks right — and the damage only appears on the next
// restart, when the ID tiebreak orders the colliding pair alphabetically
// instead of by insertion. TestAdd_ConcurrentCallsGetDistinctSortKeys pins it.
//
// A negative value is safe as the sentinel because register is the only writer
// of the column and never writes one.
const seqNext int64 = -1

// register is the only path by which a job ENTERS the registry — the sole
// writer of d.nextSeq, and the sole inserter into d.byID and d.order, which
// remove is the only other writer of (it deletes from both). Add calls it with
// seqNext to have a sequence allocated; restore calls it with the sequence the
// store recorded, which is what preserves queue order across a restart.
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
	if seq == seqNext {
		seq = d.nextSeq
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
//
// Production code uses entryFor instead — see the note there.
func (d *Dispatcher) sortKeyOf(id string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return -1
	}
	return e.seq
}

// entryFor returns everything persistIfChanged needs from the registry, in ONE
// d.mu acquisition.
//
// It replaced a headerFor call followed by a separate sortKeyOf call, which
// left a window: a job removed between the two passed the header check and then
// got sortKeyOf's not-registered sentinel, so persistIfChanged built a row with
// SortKey -1, found no matching lastWritten (remove prunes d.written too), and
// Saved a job that had just been deleted back into the store. A resurrected row
// with a negative key sorts ahead of the entire queue.
//
// The general shape is worth naming: two correct accessors composed across a
// lock boundary are not equivalent to one accessor that reads both fields.
func (d *Dispatcher) entryFor(id string) (Header, int64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return Header{}, 0, false
	}
	return e.h, e.seq, true
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
	if ch, ok := d.launched[id]; ok {
		close(ch)
		delete(d.launched, id)
	}
	// slices.Delete rather than the append-shift idiom: the shift leaves the
	// vacated tail slot holding its old string header, and d.order lives as
	// long as the Dispatcher, so a removed job's ID stayed reachable from the
	// backing array. slices.Delete zeroes what it vacates.
	if i := slices.Index(d.order, id); i >= 0 {
		d.order = slices.Delete(d.order, i, i+1)
	}
}

// Len returns the count of jobs currently registered in the queue.
func (d *Dispatcher) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.order)
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

// Row composes one job's listing entry: the header tier plus its rendered
// state, without loading or reading a manifest.
//
// It exists because List renders EVERY job through RenderAll, so using List
// for a single-job lookup trades one manifest read for an O(n) walk. That is
// #436: header-tier callers paying manifest-tier cost because the only safe
// single-job door was the expensive one.
func (d *Dispatcher) Row(id string) (Row, bool) {
	d.mu.Lock()
	e, ok := d.byID[id]
	if !ok {
		d.mu.Unlock()
		return Row{}, false
	}
	j, h := e.j, e.h
	d.mu.Unlock()

	return Row{ID: id, Header: h, View: d.q.Render(j)}, true
}

// Job returns the live *job.Job for id.
//
// It is the content-tier door, and Row (above) is the header-tier one. A
// caller that needs progress counters or the manifest takes this; a caller
// that needs a name, a status or a byte total takes Row and must not reach
// for a job pointer to get them -- that is #436 reappearing one level down.
func (d *Dispatcher) Job(id string) (*job.Job, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return nil, false
	}
	return e.j, true
}

// PauseJob asks one job to stop at its next gate.
//
// Distinct from Pause, which sets the QUEUE-wide flag. The two are different
// subjects and must stay observably different: ToSABnzbd routes through
// WaitReason.IsPause() precisely because a queue-wide pause leaves every job
// carrying IntentRun, and a PauseJob that also set the queue flag would make
// that unobservable.
func (d *Dispatcher) PauseJob(id string) error {
	j, ok := d.Job(id)
	if !ok {
		return fmt.Errorf("dispatch: pause %s: %w", id, ErrNotFound)
	}
	if err := j.SetIntent(job.IntentPause); err != nil {
		return fmt.Errorf("dispatch: pause %s: %w", id, err)
	}
	d.kick()
	return nil
}

// ResumeJob clears a pause request by restoring the default intent.
//
// It cannot un-cancel: SetIntent latches IntentCancel (job/intent.go,
// IsLatched), so this returns that error rather than silently doing nothing.
// Silently succeeding would tell the API a cancelled job had been resumed.
func (d *Dispatcher) ResumeJob(id string) error {
	j, ok := d.Job(id)
	if !ok {
		return fmt.Errorf("dispatch: resume %s: %w", id, ErrNotFound)
	}
	if err := j.SetIntent(job.IntentRun); err != nil {
		return fmt.Errorf("dispatch: resume %s: %w", id, err)
	}
	d.kick()
	return nil
}

// Remove cancels a job, waits for any in-flight worker to exit, deletes its
// persisted row and deregisters it.
//
// The order is deliberate: Cancel first so sched reclaims the lease and the
// compute slot while the job is still registered. Deregistering first would
// strand both -- the tick only walks registered jobs, so nothing would ever
// return them.
//
// Retry contract: If Remove returns an error (e.g. context cancellation while
// waiting for worker, or store failure), the job remains cancelled and
// registered, allowing the caller to retry Remove safely.
func (d *Dispatcher) Remove(ctx context.Context, id string) error {
	if _, ok := d.Job(id); !ok {
		return fmt.Errorf("dispatch: remove %s: %w", id, ErrNotFound)
	}
	if err := d.Cancel(id); err != nil {
		return fmt.Errorf("dispatch: remove %s: cancel: %w", id, err)
	}
	if err := d.waitLaunched(ctx, id); err != nil {
		return fmt.Errorf("dispatch: remove %s: wait worker: %w", id, err)
	}
	if err := d.store.Delete(ctx, id); err != nil {
		return fmt.Errorf("dispatch: remove %s: store: %w", id, err)
	}
	d.res.Evict(id)
	d.remove(id)
	d.kick()
	return nil
}

// SetWarning sets or updates the warning message for a registered job.
func (d *Dispatcher) SetWarning(id, warning string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return fmt.Errorf("dispatch: set warning %s: %w", id, ErrNotFound)
	}
	e.h.Warning = warning
	return nil
}

// SetPriority sets the priority for a registered job and kicks the scheduler.
func (d *Dispatcher) SetPriority(id string, priority int) error {
	if priority < -128 || priority > 127 || !constants.Priority(int8(priority)).IsValid() { //nolint:gosec // G115: priority fits in int8
		return fmt.Errorf("dispatch: set priority %s: invalid priority %d", id, priority)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return fmt.Errorf("dispatch: set priority %s: %w", id, ErrNotFound)
	}
	e.h.Priority = priority
	d.kick()
	return nil
}

// SetName sets the display name for a registered job.
func (d *Dispatcher) SetName(id, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return fmt.Errorf("dispatch: set name %s: %w", id, ErrNotFound)
	}
	e.h.Name = name
	e.j.SetName(name)
	return nil
}

// SetCategory sets the category for a registered job.
func (d *Dispatcher) SetCategory(id, category string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return fmt.Errorf("dispatch: set category %s: %w", id, ErrNotFound)
	}
	e.h.Category = category
	return nil
}

// SetPP sets the post-processing level for a registered job.
func (d *Dispatcher) SetPP(id string, pp int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return fmt.Errorf("dispatch: set pp %s: %w", id, ErrNotFound)
	}
	e.h.PP = pp
	e.j.SetPolicy(job.PolicyFromPP(pp))
	return nil
}

// SetScript sets the post-processing script for a registered job.
func (d *Dispatcher) SetScript(id, script string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return fmt.Errorf("dispatch: set script %s: %w", id, ErrNotFound)
	}
	e.h.Script = script
	return nil
}
