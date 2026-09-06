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

// Header is the display metadata a listing needs.
//
// Name is the one field job.Job DOES carry, and it is duplicated here on
// purpose: it lets a listing be composed from Header alone, without the
// registry handing out *job.Job pointers to do it. Header.Name is read by
// API queue search filtering (internal/api/queue.go) and Application
// name collision detection (internal/app/app.go), while downstream filesystem
// paths and finalization read Job.Name(). SetName updates both under d.mu so the
// two copies stay in lockstep.
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
	ID             string
	Header         Header
	View           job.RenderView
	RemainingBytes int64
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
	// d.order — register's append and deregister's order-preserving slices.Delete
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

	if h.Added <= 0 {
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
// deregister is the only other writer of (it deletes from both). Add calls it with
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
// SortKey -1, found no matching lastWritten (deregister prunes d.written too), and
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

// deregister deletes a job from EVERY per-job structure — d.byID, d.order,
// d.written, d.resident, d.launched, d.removing, d.occupiers, d.occupancyTokens,
// d.occupyDrained and d.occupyStep — under one d.mu span. It is the single
// internal registry eraser. External callers drain launched workers and external
// occupiers before calling, while the finalizer's own Remove proceeds with its
// own lease token active. See the Dispatcher struct's per-job map comment
// (dispatch.go) for why "every" is the rule here rather than "the ones this
// caller happens to care about".
//
// It preserves the relative order of the remaining entries: queue order is
// the priority policy sched consults, and a swap-with-last deletion would
// silently reorder jobs behind the removed one. evictCancelledNeverRun
// (tick.go) is the first caller — it guards against launched workers or active
// occupiers before calling, and this method makes no such assumption itself.
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
//   - d.occupiers / d.occupancyTokens / d.occupyDrained / d.occupyStep: a stale
//     entry leaves callers waiting on waitLive forever or reports false
//     occupancy.
//
// admitsLocked reports whether job id may take on new work: it is registered,
// and no teardown is outstanding against it. Caller must hold d.mu.
//
// This is the single read side of the removal marker, and it is one function
// rather than a repeated expression for the reason Standing Design Rule 2
// gives: the predicate had five copies (Occupy, claimLaunched,
// persistIfChanged, markResident, markWritten), and a sixth reader that got it
// subtly wrong — or a new field the invariant grows — would be invisible at
// the other five.
func (d *Dispatcher) admitsLocked(id string) bool {
	return d.byID[id] != nil && d.removing[id] == 0
}

// removal is proof that a deregistration has begun, and it is the only way to
// finish one: deregister is a method on this type and nothing else can reach
// it. That is the point of the type existing rather than a beginRemoval /
// endRemoval pair of plain functions — a pair leaves the second half callable
// on its own, so a new deregistration path can still skip the first half, and
// the failure mode of skipping it is silence rather than an error (#513).
//
// While a removal is outstanding, admitsLocked reports false for its job, so
// no occupier, worker launch, residency mark or store write attaches to a job
// that is being torn down. It ends exactly one way:
//
//   - end deregisters the job. The teardown succeeded.
//   - abort releases the marker and leaves the job registered, for a caller
//     that failed partway and means to retry.
//
// Both are idempotent and safe on a nil *removal, so a caller may defer one
// unconditionally.
//
// The marker is a COUNT, not a flag, because concurrent Remove calls for one
// ID are legal: each holds its own removal, and the job stops admitting work
// until the last of them finishes.
type removal struct {
	d    *Dispatcher
	id   string
	done bool
}

// beginRemoval marks id as being torn down and returns the proof needed to
// finish. Reports false if the job is not registered, in which case there is
// nothing to remove and no marker was set.
func (d *Dispatcher) beginRemoval(id string) (*removal, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.byID[id] == nil {
		return nil, false
	}
	return d.beginRemovalLocked(id), true
}

// beginRemovalLocked is the sole constructor of a removal — the marker is
// raised and the token minted in one place, so the two cannot drift the way
// two independently-maintained constructors of one type have here before
// (Standing Design Rule 2). Caller must hold d.mu and must already have
// established that the job is registered.
func (d *Dispatcher) beginRemovalLocked(id string) *removal {
	d.removing[id]++
	return &removal{d: d, id: id}
}

// beginRemovalIfIdle is beginRemoval for a caller that must not tear down a
// job something is already running on. It reports live if an occupier or a
// launched worker is present, in which case no marker is set and the job is
// left alone.
//
// It exists as its own door rather than as a check the caller makes first
// because the check and the marker have to be ONE d.mu acquisition. Split
// across two, the gap between them is precisely the window #513 is about, and
// a caller that got the order right would be indistinguishable from one that
// did not until something raced.
//
// Returns (nil, false) if the job is not registered — nothing to remove.
func (d *Dispatcher) beginRemovalIfIdle(id string) (rm *removal, live bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.byID[id] == nil {
		return nil, false
	}
	if len(d.occupancyTokens[id]) > 0 || d.launched[id] != nil {
		return nil, true
	}
	return d.beginRemovalLocked(id), false
}

// abort releases the marker without deregistering, returning the job to
// service. Used by a teardown that failed partway and left the job registered
// for a later retry.
func (r *removal) abort() {
	if r == nil || r.done {
		return
	}
	r.done = true
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	r.d.removing[r.id]--
	if r.d.removing[r.id] <= 0 {
		delete(r.d.removing, r.id)
	}
}

// end deregisters the job, completing the teardown.
func (r *removal) end() {
	if r == nil || r.done {
		return
	}
	r.done = true
	r.d.deregister(r.id)
}

// deregister erases every trace of a job from the registry.
//
// removal.end is its only caller, which is what makes the marker impossible
// to skip: `git grep -n 'deregister(r\.id)' -- 'internal/dispatch/*.go'` finds
// 1 line, end's own call. The pattern carries the argument because the bare
// call spelling matches this sentence too, and a citation that counts its own
// prose is not a check.
// The path in is beginRemoval or beginRemovalIfIdle,
// both of which raise the marker before minting the token this needs, so
// there is no spelling of "deregister without marking" for a new caller to
// reach for — the gap #513 was.
//
// What the citation does NOT cover is a test calling this directly — the
// pattern is anchored on end's argument, so `d.deregister("j1")` would not
// match it. That is deliberate rather than an oversight:
// TestDeregister_IsTotal does exactly that, because the property worth
// pinning here is that this clears EVERY per-job map, and reaching it through
// the token would test end instead.
func (d *Dispatcher) deregister(id string) {
	d.mu.Lock()
	delete(d.byID, id)
	delete(d.written, id)
	delete(d.resident, id)
	delete(d.removing, id)
	delete(d.occupiers, id)
	delete(d.occupancyTokens, id)
	if ch, ok := d.occupyDrained[id]; ok {
		close(ch)
		delete(d.occupyDrained, id)
	}
	if ch, ok := d.occupyStep[id]; ok {
		close(ch)
		delete(d.occupyStep, id)
	}
	// slices.Delete rather than the append-shift idiom: the shift leaves the
	// vacated tail slot holding its old string header, and d.order lives as
	// long as the Dispatcher, so a removed job's ID stayed reachable from the
	// backing array. slices.Delete zeroes what it vacates.
	if i := slices.Index(d.order, id); i >= 0 {
		d.order = slices.Delete(d.order, i, i+1)
	}
	d.mu.Unlock()
	d.clearLaunched(id)
}

// Len returns the count of jobs currently registered in the queue.
func (d *Dispatcher) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.order)
}

// List composes the queue listing. It takes Queue.mu exactly once, through
// RenderAll, so every row's scheduler StateView is from one instant.
// RemainingBytes is read per job from live progress under contentMu.RLock.
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
		rem := hs[i].Bytes
		if js[i] != nil && js[i].HasProgress() {
			rem = js[i].RemainingBytes()
		}
		out = append(out, Row{
			ID:             id,
			Header:         hs[i],
			View:           views[i],
			RemainingBytes: rem,
		})
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

	rem := h.Bytes
	if j.HasProgress() {
		rem = j.RemainingBytes()
	}
	return Row{ID: id, Header: h, View: d.q.Render(j), RemainingBytes: rem}, true
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

// Remove cancels a job, waits for its launch claim latch to be cleared via
// waitLaunched, deletes its persisted row and deregisters it.
//
// The order is deliberate: Cancel first so sched reclaims the lease and the
// compute slot while the job is still registered. Deregistering first would
// strand both -- the tick only walks registered jobs, so nothing would ever
// return them.
//
// Retry contract: If Remove returns an error (e.g. context cancellation while
// waiting for worker, or store failure), the job remains cancelled and
// registered, allowing the caller to retry Remove safely.
// The named return is what lets the marker be released in ONE place. Every
// error path here leaves the job registered and retryable, so every error path
// owed an identical decrement — five copies of it before #513, which is the
// shape Rule 2 calls a second enforcement point for one invariant. The defer
// below is the enforcement point; adding a sixth error path now costs nothing
// and cannot forget.
func (d *Dispatcher) Remove(ctx context.Context, id string) (err error) {
	rm, ok := d.beginRemoval(id)
	if !ok {
		return fmt.Errorf("dispatch: remove %s: %w", id, ErrNotFound)
	}
	defer func() {
		if err != nil {
			rm.abort()
		}
	}()

	if err := d.Cancel(id); err != nil {
		return fmt.Errorf("dispatch: remove %s: cancel: %w", id, err)
	}
	if err := d.waitLaunched(ctx, id); err != nil {
		return fmt.Errorf("dispatch: remove %s: wait worker: %w", id, err)
	}
	if tok, ok := ctx.Value(occupyContextKey{}).(occupyToken); ok && tok.id == id {
		if err := d.waitLiveExcept(ctx, id, tok.token); err != nil {
			return fmt.Errorf("dispatch: remove %s: wait live: %w", id, err)
		}
	} else {
		if err := d.waitLive(ctx, id); err != nil {
			return fmt.Errorf("dispatch: remove %s: wait live: %w", id, err)
		}
	}
	d.storeMu.Lock()
	delErr := d.store.Delete(ctx, id)
	d.storeMu.Unlock()
	if delErr != nil {
		return fmt.Errorf("dispatch: remove %s: store: %w", id, delErr)
	}
	d.res.Evict(id)
	rm.end()
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

// SetName updates the job's display and filesystem name across both the Header
// (used for listings without job pointer dereference) and the Job instance (used for
// downstream filesystem paths and finalization) atomically under d.mu.
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
