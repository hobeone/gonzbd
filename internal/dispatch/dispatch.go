package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/sched"
)

// Dispatcher drives sched.Queue. It owns the Queue outright (D-B13): a caller
// reaching sched.Cancel directly would latch the cancel intent and skip the
// eviction Cancel below performs, reintroducing the defect where a deleted job
// renders as queued forever.
//
// Task 2 declares every field the dispatcher will ever hold, before every
// field had a reader — Go permits unused struct fields, and the alternative,
// adding fields piecemeal per task, would leave this file's own test
// constructor unable to compile until the last task landed. Every field now
// has at least one reader: res and store since Task 4 and Task 7's restore,
// runner since Task 5's launch, tickEvery/stop/done/started since Task 3's
// Start/Stop/run, log since the log helpers below, and written — the last to
// gain one — since Task 7's persistIfChanged (write) and restore (seed).
type Dispatcher struct {
	mu    sync.Mutex
	byID  map[string]*entry
	order []string

	// resident, launched and written are the dispatcher's own bookkeeping,
	// all guarded by mu. None may be held across a call into sched or into
	// Residency.Hydrate — take mu, read or write one map, release.
	resident map[string]bool
	launched map[string]bool
	written  map[string]Persisted

	q    *sched.Queue
	wake chan struct{}

	res    Residency
	store  Store
	runner Runner

	tickEvery time.Duration
	stop      chan struct{}
	done      chan struct{}
	started   bool

	log *slog.Logger
}

// lookup returns the registered job for an ID. Task 5's Cancel and Task 6's
// eviction are its production callers; declared here because it is
// registry-shaped scaffolding, not tick behaviour.
func (d *Dispatcher) lookup(id string) (*job.Job, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return nil, false
	}
	return e.j, true
}

// Cancel latches the intent through sched and wakes the tick.
//
// It exists on Dispatcher rather than leaving callers to reach sched.Cancel
// because D-B12's eviction has no other home: sched has no registry to remove a
// never-run job from. A second route to the latch would skip it. Task 6 adds
// the eviction; here it delegates and kicks.
func (d *Dispatcher) Cancel(id string) error {
	j, ok := d.lookup(id)
	if !ok {
		return fmt.Errorf("dispatch: Cancel: no job %q", id)
	}
	if err := d.q.Cancel(j); err != nil {
		return fmt.Errorf("dispatch: Cancel(%s): %w", id, err)
	}
	d.kick()
	return nil
}

// Retry re-arms a settled job for another attempt through sched and wakes the
// tick.
func (d *Dispatcher) Retry(id string) error {
	j, ok := d.lookup(id)
	if !ok {
		return fmt.Errorf("dispatch: Retry: no job %q", id)
	}
	if err := d.q.Retry(j); err != nil {
		return fmt.Errorf("dispatch: Retry(%s): %w", id, err)
	}
	d.kick()
	return nil
}

// Pause sets the Queue's pause flag and wakes the tick (D-B13).
func (d *Dispatcher) Pause() { d.q.Pause(); d.kick() }

// Resume clears the Queue's pause flag and wakes the tick (D-B13).
func (d *Dispatcher) Resume() { d.q.Resume(); d.kick() }

// Paused reports the Queue's pause flag (D-B13).
func (d *Dispatcher) Paused() bool { return d.q.Paused() }

// The three log helpers exist so the tick has exactly one shape for "this job
// failed, keep walking". A tick must never abandon the rest of the queue
// because one job errored — that would let a single bad job stall every other,
// which is the blast radius Standing Design Rule 3 bounds for articles and the
// same argument applies here. Production callers arrive with the tick: Task
// 3's tick (logAdvanceError), Task 4's reconcileResidency
// (logResidencyError), and Task 6's eviction plus Task 7's persistIfChanged
// (logStoreError).
func (d *Dispatcher) logAdvanceError(id string, err error) {
	d.log.Error("advance failed", "job_id", id, "err", err)
}

func (d *Dispatcher) logResidencyError(id string, err error) {
	d.log.Error("residency reconcile failed", "job_id", id, "err", err)
}

func (d *Dispatcher) logStoreError(id string, err error) {
	d.log.Error("store write failed", "job_id", id, "err", err)
}

// kick wakes the ticker without blocking. The channel is buffered to 1, so a
// burst of Adds collapses into one wakeup and a full buffer means a wakeup is
// already pending.
func (d *Dispatcher) kick() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// New builds a Dispatcher and the sched.Queue it owns.
//
// It panics on a nil Residency, Store, Workers or Runner for the same reason
// sched.New panics on a nil Workers: these are construction-time programmer
// errors, not state an earlier build wrote, so Standing Design Rule 1's
// guard-removal argument does not apply. Failing here beats a nil dereference
// on the ticker goroutine with no construction frame left to explain it — the
// Runner check in particular is what stands between a missing wiring and
// launch's `d.runner.Run` panicking on the first job that renders Running.
func New(leaseCap, slotCap int, tickEvery time.Duration, clock func() time.Time, w sched.Workers, r Residency, s Store, run Runner) *Dispatcher {
	if w == nil {
		panic("dispatch: New: Workers must not be nil")
	}
	if r == nil {
		panic("dispatch: New: Residency must not be nil")
	}
	if s == nil {
		panic("dispatch: New: Store must not be nil")
	}
	if run == nil {
		panic("dispatch: New: Runner must not be nil")
	}
	if tickEvery <= 0 {
		panic("dispatch: New: tick interval must be positive")
	}
	return &Dispatcher{
		byID:      map[string]*entry{},
		resident:  map[string]bool{},
		launched:  map[string]bool{},
		written:   map[string]Persisted{},
		q:         sched.New(leaseCap, slotCap, clock, w),
		wake:      make(chan struct{}, 1),
		res:       r,
		store:     s,
		runner:    run,
		tickEvery: tickEvery,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		log:       slog.Default(),
	}
}

// Start registers everything the store holds, then launches the ticker and
// returns. It does not block.
//
// A second Start is an error rather than a no-op: it would create a second
// ticker goroutine, and two concurrent walks would need locking between them
// that D-B7's single-goroutine design deliberately does not have.
func (d *Dispatcher) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return errors.New("dispatch: Start: already started")
	}
	d.started = true
	d.mu.Unlock()

	if err := d.restore(ctx); err != nil {
		// The goroutine never launches on this path, so d.done never closes.
		// Leaving d.started true here would make a later Stop() read
		// wasStarted true, close d.stop, and then block forever on <-d.done
		// with nothing left to close it — a real deadlock, not a hypothetical
		// one. Clearing it under mu is what makes Stop's wasStarted read see
		// a Dispatcher that, as far as Stop is concerned, was never started.
		d.mu.Lock()
		d.started = false
		d.mu.Unlock()
		return fmt.Errorf("dispatch: Start: %w", err)
	}

	go d.run(ctx)
	return nil
}

// run is the ticker goroutine (D-B7): it owns liveness, walking the registry
// on every tick and on every kick, until told to stop.
//
// The ticker and the kick share one call to tick below rather than each
// having its own: both mean the same thing to this loop — "walk the
// registry now" — and giving them separate call sites would make the wake
// path a second, easy-to-miss route to the same behaviour the ticker
// already provides on its own (D-B7).
func (d *Dispatcher) run(ctx context.Context) {
	defer close(d.done)
	t := time.NewTicker(d.tickEvery)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
		d.tick(ctx)
	}
}

// Stop halts the ticker, waits for the in-flight tick to finish, and parks
// every job that still holds resources.
//
// It parks rather than settles. A shutdown is not an outcome: recording one
// would claim a verdict about work that simply stopped, and an Outcome must
// not contradict what is on disk. Park is safe for every shape because it is
// unconditional and total.
//
// The channel close/wait dance is gated on started, but the parking sweep is
// NOT: Stop must return every held resource even for a Dispatcher whose
// ticker was never started (a caller that only ever drove the queue through
// direct tick calls, as this package's own tests do), and it must be safe to
// call twice. Park and Residency.Evict are both unconditional and idempotent
// (job.Surrender on an already-parked job returns a nil lease, which reclaim
// treats as a no-op — see sched.leasePool.reclaim), so re-running the sweep
// on a second call costs nothing and cannot double-release.
//
// started itself IS the idempotency guard for the channel dance: it is read
// and cleared under mu in one step, so only the call that observes it true
// closes d.stop and waits on d.done. A second call — concurrent or
// sequential — finds it already false and skips straight to the sweep,
// which never touches d.stop or d.done.
//
// The sweep also clears each job's launched claim, alongside Park and Evict.
// Stop is a third worker-exit path beside Finished and Yielded — the ticker
// goroutine has already stopped by the time the sweep runs (the wait above),
// so nothing races a launch here — and it must clear the same bookkeeping
// they do. Without this, a job that held a claim when Stop ran would still
// carry it after a later Start reopens the Dispatcher, and claimLaunched
// would refuse it forever: the job would render Running-eligible but never
// actually launch.
func (d *Dispatcher) Stop() error {
	d.mu.Lock()
	wasStarted := d.started
	d.started = false
	d.mu.Unlock()

	if wasStarted {
		close(d.stop)
		<-d.done
	}

	var firstErr error
	for _, j := range d.snapshotOrder() {
		if err := d.q.Park(j); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("dispatch: Stop: park %s: %w", j.ID(), err)
		}
		d.res.Evict(j.ID())
		d.clearLaunched(j.ID())
	}
	return firstErr
}

// restore registers everything the store holds, before the first tick.
//
// It runs synchronously inside Start, before the ticker goroutine launches —
// `go d.run(ctx)` is Start's next line once this call succeeds — so it needs
// no locking against a concurrent tick: there is not one yet.
//
// Every job comes back holding nothing, whatever position it was persisted
// at. The pools are process-local: there is no lease or slot from a previous
// process to reclaim, so a job persisted mid-Repairing is simply a job at
// Repairing that holds nothing, and branch 2 of Advance grants it resources
// on the first tick — the same path a paused job resumes through (D-B13's
// Startup paragraph, docs/superpowers/specs/2026-08-28-sched-dispatcher-design.md).
//
// Standing Design Rule 1 applies directly: rows an earlier build wrote may be
// assumed to satisfy the invariants this design introduces, so there is no
// migration, no dual-read, and no "old jobs behave differently" branch.
//
// Each row is rebuilt by reconstruct, which replays it forward through
// job.Job's own doors rather than through a second constructor — see
// reconstruct's comment for why that beats a job.Restore(...) the brief first
// proposed. A restored row is marked written immediately: it is, by
// definition, what the store already holds, so the first tick's
// persistIfChanged has nothing new to say about it and costs no store
// traffic until the job actually moves.
func (d *Dispatcher) restore(ctx context.Context) error {
	rows, err := d.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: restore: load: %w", err)
	}
	now := time.Now()
	for _, p := range rows {
		j, err := reconstruct(p.ID, p.Header.Name, p.State, p.Intent, now)
		if err != nil {
			return fmt.Errorf("dispatch: restore: job %s at %+v: %w", p.ID, p.State, err)
		}
		if err := d.Add(j, p.Header); err != nil {
			return fmt.Errorf("dispatch: restore: register %s: %w", p.ID, err)
		}
		d.markWritten(p)
	}
	return nil
}

// hop is one edge of reconstruct's replay: transition (or cross, for the
// one edge that requires it) to `to`.
type hop struct {
	to    job.State
	cross bool
}

// takeHop takes one hop: SetNext(to) then Cross(to) when cross is set —
// Cross refuses to move without a recorded destination — or a plain
// Transition(to) otherwise. It is the one place either door is called during
// replay, so replayPath's table can name a hop as data instead of repeating
// this two-line dance at every site that needs the crossing edge.
func takeHop(j *job.Job, h hop) error {
	if h.cross {
		if err := j.SetNext(h.to); err != nil {
			return err
		}
		_, err := j.Cross(h.to)
		return err
	}
	return j.Transition(h.to)
}

// replayPath is the canonical hop sequence from Fetching — where
// reconstruct's BeginAttempt opens — to each State other than Fetching
// itself. Assessing -> Extracting is the one byCross edge (legalEdges,
// internal/job/transition.go), so it is the only hop in this table with
// cross set; every other hop is an ordinary Transition.
//
// Fetching has no entry: reconstruct resolves it separately, since reaching
// Fetching from Fetching depends on v.Assessed (a persisted field, not just
// v.State) rather than on a fixed hop count.
var replayPath = map[job.State][]hop{
	job.Assessing:  {{job.Assessing, false}},
	job.Repairing:  {{job.Assessing, false}, {job.Repairing, false}},
	job.Extracting: {{job.Assessing, false}, {job.Extracting, true}},
	job.Finalizing: {{job.Assessing, false}, {job.Extracting, true}, {job.Finalizing, false}},
}

// reconstruct rebuilds a *job.Job at a persisted position by replaying it
// forward through job.Job's own doors, starting from job.New — never through
// a second constructor.
//
// The brief this task started from called a `job.Restore(id, name, Policy{},
// state, intent)` that does not exist. internal/job exports exactly one
// constructor, New (job.go:205); adding a second is the first smell Standing
// Design Rule 2 names, with newManifest/UnmarshalJSON as the worked example
// that had already diverged over totalBytes before anyone noticed. That was
// escalated per AGENTS.md's Decision Protocol and declined. Replaying instead
// gets three things a second constructor cannot:
//
//  1. No second write path, so nothing can diverge — Rule 2 holds
//     structurally, not because a comment promises New and a restore path
//     agree.
//  2. The state machine validates the persisted data for free: a row naming
//     an illegal position, an inadmissible Outcome for its State, or a Next
//     that is not a legal edge is refused by the door itself (SetNext,
//     Transition, Cross or Finish returning their ordinary sentinel), rather
//     than being trusted into memory.
//  3. Boundary consumption comes out right by construction: reaching
//     Extracting or Finalizing goes through Cross, which is the one edge
//     legalEdges marks byCross (internal/job/transition.go), so a restored
//     job that had crossed reports ErrBoundaryConsumed to a later Retry
//     exactly as a job that crossed in this process would.
//
// Known limitation, not a bug to fix: a restored job reports exactly one
// attempt (j.Attempts() == 1) regardless of how many attempts it actually
// ran before this process restarted. Persisted carries only the current
// State/Next/Activity/Outcome/Assessed — no attempt history — so replay loses
// nothing that was actually stored; there is no attempt-count field to
// reconstruct from.
//
// v.Assessed is consulted only for v.State == Fetching, to decide whether the
// replay must round-trip through Assessing first (a job that has already been
// assessed and re-entered Fetching, e.g. after a Repairing verdict, vs. one
// that has never left it). For every other State, entering it forces
// Assessed true by the same mechanism a live job would go through
// (transition() sets it on the way to Assessing), so v.Assessed is not
// separately checked there — there is no door that could set it
// independently, and a row whose Assessed disagrees at those states is not
// reachable in the first place.
func reconstruct(id, name string, v job.StateView, intent job.Intent, now time.Time) (*job.Job, error) {
	j := job.New(id, name, job.Policy{})

	if v.State == job.StateUnset {
		// Never ran: no attempt to open. This package never persists a row
		// shaped like this with anything else set — persistIfChanged always
		// renders State from a live job.Job, and a never-run job's Next,
		// Activity and Outcome are all their zero values — so a row that
		// claims otherwise names a position no attempt could have produced.
		if v.Next != job.StateUnset || v.Activity != job.ActNone || v.Outcome != job.OutcomePending {
			return nil, fmt.Errorf("job %s: StateUnset row carries %+v, which no attempt can hold", id, v)
		}
		if err := j.SetIntent(intent); err != nil {
			return nil, fmt.Errorf("job %s: SetIntent(%s): %w", id, intent, err)
		}
		return j, nil
	}

	if err := j.BeginAttempt(now); err != nil {
		return nil, fmt.Errorf("job %s: BeginAttempt: %w", id, err)
	}

	// Walk the canonical path from Fetching, where BeginAttempt opened, to
	// v.State. replayPath names every hop but Fetching's own: reaching
	// Fetching from Fetching is zero hops unless v.Assessed says the job has
	// already been through Assessing and back, which is resolved here rather
	// than in the table since it is the one place a persisted field (not
	// just v.State) decides the path.
	path, ok := replayPath[v.State]
	switch {
	case v.State == job.Fetching && v.Assessed:
		path = []hop{{job.Assessing, false}, {job.Fetching, false}}
	case v.State == job.Fetching:
		path = nil
	case !ok:
		return nil, fmt.Errorf("job %s: %s is not a declared State", id, v.State)
	}
	for _, h := range path {
		if err := takeHop(j, h); err != nil {
			return nil, fmt.Errorf("job %s: replay ->%s: %w", id, h.to, err)
		}
	}

	// The moves above always land with next cleared (transition and cross
	// both clear it on every hop), so a recorded verdict is restored here,
	// once, rather than threaded through each case above. SetNext validates
	// it against v.State via CanTransition — an edge v.State does not carry
	// is refused here rather than silently dropped.
	if v.Next != job.StateUnset {
		if err := j.SetNext(v.Next); err != nil {
			return nil, fmt.Errorf("job %s: replay SetNext(%s): %w", id, v.Next, err)
		}
	}
	if v.Activity != job.ActNone {
		if err := j.SetActivity(v.Activity); err != nil {
			return nil, fmt.Errorf("job %s: SetActivity(%s): %w", id, v.Activity, err)
		}
	}
	// Finish last among the attempt mutators: it is write-once and clears
	// Next/Activity itself, so calling it before SetNext/SetActivity above
	// would either be overwritten or refuse a settled attempt outright.
	if v.Outcome != job.OutcomePending {
		if _, err := j.Finish(v.Outcome, now); err != nil {
			return nil, fmt.Errorf("job %s: replay Finish(%s): %w", id, v.Outcome, err)
		}
	}
	// SetIntent last, per the brief: a cancelled job's latch must not gate
	// its own replay above, which drives the job through ordinary work-spine
	// doors regardless of Intent.
	if err := j.SetIntent(intent); err != nil {
		return nil, fmt.Errorf("job %s: SetIntent(%s): %w", id, intent, err)
	}
	return j, nil
}

// headerFor returns the Header a registered job was added with. persistIfChanged
// (tick.go) is the caller: it needs the Header to build the Persisted row
// Render alone cannot supply, since job.Job carries only id, name and policy.
func (d *Dispatcher) headerFor(id string) (Header, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	if !ok {
		return Header{}, false
	}
	return e.h, true
}

// lastWritten and markWritten are persistIfChanged's two touches of d.written,
// each taking d.mu for one map operation and releasing it immediately. D-B9
// forbids holding d.mu across the Render/Save calls between them — see
// persistIfChanged (tick.go).
func (d *Dispatcher) lastWritten(id string) (Persisted, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.written[id]
	return p, ok
}

func (d *Dispatcher) markWritten(p Persisted) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.written[p.ID] = p
}
