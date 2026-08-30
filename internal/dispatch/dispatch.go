package dispatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
// res and store are read by restore and reconcileResidency; runner is read by
// launch; tickEvery/stop/done/started/stopped drive Start/Stop/run; log backs
// the log helpers below; written is persistIfChanged's and restore's record
// of what the store already holds.
type Dispatcher struct {
	mu    sync.Mutex
	byID  map[string]*entry
	order []string

	// resident, launched and written are the dispatcher's own per-job
	// bookkeeping, all guarded by mu. None may be held across a call into
	// sched or into Residency.Hydrate — take mu, read or write one map,
	// release.
	//
	// ADDING A MAP HERE MEANS EXTENDING EVERY TEARDOWN. This has been got
	// wrong three times on this branch — Stop not clearing resident, remove
	// not clearing written, remove not clearing resident and launched — and
	// each time the shape was identical: a new per-job map arrived and an
	// existing teardown was not extended. The failure is silent, because a
	// stale entry only bites a job ID that comes back (a reused ID reads as
	// already resident, so it never hydrates, and as already launched, so it
	// is never launchable).
	//
	// The teardowns, enumerated from source rather than remembered —
	// `grep -n 'delete(d\.' internal/dispatch/*.go` finds six lines:
	//
	//   - remove (registry.go), four lines: byID, written, resident,
	//     launched. It is total by rule; d.order is pruned by the loop
	//     below those four rather than by a delete, so it does not appear.
	//   - markNotResident (tick.go), one line: resident. The per-map
	//     accessor reconcileResidency and Stop's sweep both call.
	//   - clearLaunched (worker.go), one line: launched. The per-map
	//     accessor Finished, Yielded and Stop's sweep all call.
	//
	// Stop's sweep therefore prunes resident and launched through those two
	// accessors; it deliberately leaves byID, order and written intact,
	// because a Stopped Dispatcher is still inspectable and its jobs still
	// exist. remove is the only site that must be total.
	resident map[string]bool
	launched map[string]bool
	written  map[string]Persisted

	// nextSeq is the sequence register will hand the next job. register is
	// its only writer: `git grep -n 'd\.nextSeq = ' internal/dispatch` returns
	// 1 line, register's own max(). Add reads it and passes the value on, but
	// does not advance it.
	nextSeq int64

	q    *sched.Queue
	wake chan struct{}

	res    Residency
	store  Store
	runner Runner

	tickEvery time.Duration
	stop      chan struct{}
	done      chan struct{}
	started   bool
	stopped   bool

	// stopOnce and stopErr make Stop total for CONCURRENT callers, which the
	// started/stopped latches alone cannot: those are read under d.mu and
	// cleared there, so a second Stop entering while the first is still
	// waiting on d.done reads wasStarted false and would skip the wait.
	// Every caller now blocks on the same Do and observes the same result.
	stopOnce sync.Once
	stopErr  error

	log *slog.Logger
}

// lookup returns the registered job for an ID. Its production callers are
// Cancel and Retry below, the only two non-test call sites in this package;
// declared here because it is registry-shaped scaffolding, not tick
// behaviour.
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
// never-run job from. A second route to the latch would skip it. The eviction
// itself runs later in the same tick, from evictCancelledNeverRun (tick.go);
// here Cancel only latches the intent through sched and kicks.
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
// same argument applies here. Production callers, all in tick.go: tick itself
// (logAdvanceError), reconcileResidency (logResidencyError), and
// evictCancelledNeverRun plus persistIfChanged (logStoreError).
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
//
// A Dispatcher that has been Stopped cannot be restarted: Stop is terminal.
// d.stop and d.done are created once, in New, and closing d.stop is
// irreversible — recreating them on a later Start would add a reuse path with
// its own races (a worker goroutine still unwinding from the old d.done close
// racing a new Start's d.restore, for one) to serve a capability nothing in
// this codebase asks for: B2.4 constructs one Dispatcher per process. This
// matches net/http.Server, whose Shutdown is documented as making the server
// unusable for future calls.
func (d *Dispatcher) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return errors.New("dispatch: Start: this Dispatcher was already stopped and cannot be restarted")
	}
	if d.started {
		d.mu.Unlock()
		return errors.New("dispatch: Start: already started")
	}
	d.started = true
	d.mu.Unlock()

	if err := d.restore(ctx); err != nil {
		// The goroutine never launches on this path, so run's deferred
		// close(d.done) never runs. Leaving d.started true here would make a
		// LATER Stop() read wasStarted true, close d.stop, and then block
		// forever on <-d.done with nothing left to close it. Clearing it
		// under mu is what makes such a Stop's wasStarted read see a
		// Dispatcher that, as far as Stop is concerned, was never started.
		//
		// Clearing started is not enough for a Stop that is ALREADY waiting.
		// restore does Store I/O and D-B9 forbids holding d.mu across it, so
		// the whole of restore runs unlocked and a concurrent Stop can pass
		// through its own critical section in that window: it reads
		// wasStarted true (this Start set it), latches stopped, closes d.stop
		// and blocks on <-d.done. Nothing else will ever close d.done. So
		// this path closes it when — and only when — it observes that latch.
		//
		// d.done is closed exactly once. The argument (approach (a): a proof,
		// not a sync.Once, because the disjointness is structural and a Once
		// here would hide rather than establish it):
		//
		//  1. run's deferred close runs only if `go d.run(ctx)` below was
		//     reached. There are now THREE closers, not two, because the
		//     succeeding-restore path below closes d.done when it finds
		//     stopped latched — so "restore returned nil" no longer implies
		//     run was launched. They stay disjoint: this branch runs only on
		//     a non-nil restore and that one only on a nil restore, and that
		//     one returns BEFORE `go d.run(ctx)` when it closes. Start
		//     therefore takes exactly one of the three per call.
		//  2. This branch cannot run twice. stopped is a one-way latch (Stop
		//     sets it, nothing clears it), and Start's first check refuses
		//     any call that sees it set. A second Start therefore reaches
		//     restore only while stopped is still false — and a Start that
		//     reaches this branch while stopped is false does not close.
		//     Concretely: the goroutine that closes read stopped true inside
		//     the critical section below, so every Start entering afterwards
		//     is refused at that first check, and every Start that entered
		//     earlier either saw started true (this call's claim) and was
		//     refused, or takes d.mu after this section and sees stopped
		//     true. Either way, no second close.
		d.mu.Lock()
		d.started = false
		sawStop := d.stopped
		d.mu.Unlock()
		if sawStop {
			close(d.done)
		}
		return fmt.Errorf("dispatch: Start: %w", err)
	}

	// restore ran unlocked, so a Stop can have landed inside it. Without this
	// check Start would launch run and return nil for a Dispatcher that is
	// already stopped — naming a success that did not happen, and handing run
	// a d.stop that is ALREADY closed. That last part is the sharp end: Add's
	// kick may have primed d.wake, and run's select would then have two ready
	// cases, which Go chooses between uniformly at random. It could take the
	// wake, tick, and launch workers after shutdown.
	//
	// Closing d.done here is what lets the Stop blocked on it return; nothing
	// else would, since run is deliberately not launched. See the
	// closed-exactly-once argument above, which this path is the third arm of.
	// started stays SET across this check. Clearing it and restoring it would
	// open a window in which a concurrent Stop reads wasStarted false, skips
	// its wait on d.done, and returns while run is about to be launched.
	// One critical section, and started is cleared only on the bail-out.
	d.mu.Lock()
	sawStop := d.stopped
	if sawStop {
		d.started = false
	}
	d.mu.Unlock()
	if sawStop {
		close(d.done)
		return errors.New("dispatch: Start: stopped while restoring; this Dispatcher is terminal")
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
		// Shutdown takes priority over work. A select with several ready
		// cases picks one uniformly at random, so a closed d.stop and a
		// primed d.wake are a coin toss — and losing it means ticking, and
		// launching workers, after shutdown. Add's kick primes d.wake, and a
		// Stop can land between Start's stopped check and this loop's first
		// pass, so both really are ready together. This non-blocking pass
		// settles it before the blocking select below can gamble.
		select {
		case <-d.stop:
			return
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-d.stop:
			return
		case <-ctx.Done():
			// This exit leaves d.started true with no goroutine behind it,
			// so a later Start refuses forever (it never sees d.stop close,
			// since only Stop closes that channel). That is deliberate, not
			// an oversight: nothing but Stop makes a Dispatcher usable again
			// in the first place, now that Stop is terminal, so ctx
			// cancellation without an explicit Stop simply leaves the
			// Dispatcher inert rather than restartable. A caller that wants
			// a clean, inspectable shutdown after cancelling ctx still calls
			// Stop — see TestRun_AdvancesOnAWakeAndStopsOnContextCancel.
			return
		case <-t.C:
		case <-d.wake:
		}
		d.tick(ctx)
	}
}

// Stop halts the ticker, waits for the in-flight tick to finish, parks every
// job that still holds resources, and evicts its manifest residency. It is
// terminal: a Dispatcher that has been Stopped cannot be Started again (see
// Start's doc comment).
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
// call twice. Park, Residency.Evict and markNotResident are all unconditional
// and idempotent (job.Surrender on an already-parked job returns a nil lease,
// which reclaim treats as a no-op — see sched.leasePool.reclaim; delete on an
// absent map key is a no-op), so re-running the sweep on a second call costs
// nothing and cannot double-release.
//
// started itself IS the idempotency guard for the channel dance: it is read
// and cleared under mu in one step, so only the call that observes it true
// closes d.stop and waits on d.done. A second call — concurrent or
// sequential — finds it already false and skips straight to the sweep,
// which never touches d.stop or d.done. stopped is a separate, one-way latch:
// it is set on every call (there is no "already stopped" branch to take),
// because Stop's terminal contract holds regardless of how many times it is
// called or whether the ticker was ever running.
//
// The sweep also clears each job's launched claim and its manifest
// residency, alongside Park and Evict. Stop is a third worker-exit path
// beside Finished and Yielded — the ticker goroutine has already stopped by
// the time the sweep runs (the wait above), so nothing races a launch here —
// and it must clear the same bookkeeping they do. Now that Stop is terminal,
// this is no longer about surviving a later Start: it is about leaving the
// Dispatcher's own bookkeeping consistent and inspectable after shutdown,
// since this package's tests (and any caller keeping a Stopped Dispatcher
// around to inspect) drive tick and its helpers directly, and a stale
// launched or resident entry would be a lie about what the Dispatcher
// believes is true. Without the resident clear specifically, a job that held
// a lease when Stop ran would still read as manifest-resident afterward, so a
// direct tick() call reaching reconcileResidency for it (v.Holds is now
// false, post-Park) would see d.isResident true and correctly evict — but
// only because reconcileResidency's own else-branch happens to cover it; the
// job would never re-hydrate, because the branch that grants residency
// requires !d.isResident(id) and this entry would already, wrongly, satisfy
// that.
func (d *Dispatcher) Stop() error {
	// The whole sequence runs once, and every caller waits for it. Reading
	// the latches under d.mu is enough to make Stop IDEMPOTENT in sequence
	// but not total under concurrency: the first caller clears started before
	// it blocks on d.done, so a second caller arriving in that window read
	// wasStarted false, skipped the wait, and ran the teardown sweep — Park,
	// Evict, markNotResident, clearLaunched — against a tick that was still
	// calling Advance, Hydrate and Runner.Run. It then returned nil, naming a
	// shutdown that had not happened.
	//
	// sync.Once gives both halves at once: the sweep cannot run twice, and Do
	// blocks every later caller until the first has finished, so no Stop can
	// return early. Do also establishes the happens-before edge that makes
	// reading stopErr after it safe without d.mu.
	//
	// This does not add a closer of d.done — Stop only ever waits on it — so
	// Start's closed-exactly-once argument is untouched.
	d.stopOnce.Do(func() {
		d.mu.Lock()
		wasStarted := d.started
		d.started = false
		d.stopped = true
		d.mu.Unlock()

		if wasStarted {
			close(d.stop)
			<-d.done
		}

		for _, j := range d.snapshotOrder() {
			if err := d.q.Park(j); err != nil && d.stopErr == nil {
				d.stopErr = fmt.Errorf("dispatch: Stop: park %s: %w", j.ID(), err)
			}
			d.res.Evict(j.ID())
			d.markNotResident(j.ID())
			d.clearLaunched(j.ID())
		}
	})
	return d.stopErr
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
	// Restore is all-or-nothing. Registering as it goes would otherwise leave
	// every row before the failing one in the registry: Start reports the
	// error and clears started, so a caller may legitimately retry once a
	// transient store problem clears — but the retry re-Loads the same rows
	// and Add refuses the first with "already registered", so the dispatcher
	// can never start again. In between, List and Stop would be operating on
	// a queue that was never fully restored.
	//
	// remove is total (registry.go), so rolling back a partial restore needs
	// nothing beyond calling it for each ID this call registered — including
	// the write markWritten recorded, which remove also clears.
	// Sort rather than trust Load's slice order, with a tiebreak matching the
	// SQLite store's `ORDER BY sort_key ASC, id ASC`. A sort keyed on SortKey
	// alone disagrees with SQLite whenever two keys collide, and the
	// disagreement is non-deterministic. Keeping the sort here also means
	// Load's ordering is a performance property rather than a correctness
	// one, which is the safer place for it.
	slices.SortStableFunc(rows, func(a, b Persisted) int {
		if c := cmp.Compare(a.SortKey, b.SortKey); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})

	now := time.Now()
	var registered []string
	defer func() {
		for _, id := range registered {
			d.remove(id)
		}
	}()
	for _, p := range rows {
		j, err := reconstruct(p.ID, p.Header.Name, p.Policy, p.State, p.Intent, now)
		if err != nil {
			return fmt.Errorf("dispatch: restore: job %s at %+v: %w", p.ID, p.State, err)
		}
		// register, not Add: Add would assign a FRESH sequence while
		// markWritten below records the stored one, and the first
		// persistIfChanged would then see a difference that is not there and
		// rewrite every restored row's key.
		// TestRestore_DoesNotRewriteRowsItJustRead pins that.
		if err := d.register(j, p.Header, p.SortKey); err != nil {
			return fmt.Errorf("dispatch: restore: register %s: %w", p.ID, err)
		}
		registered = append(registered, p.ID)
		d.markWritten(p)
	}
	registered = nil // every row landed; the deferred rollback becomes a no-op
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
		// Cross returns the lease it surrenders, and replay has none to
		// surrender: Cross yields j.surrenderLocked() (internal/job/job.go),
		// which returns j.lease, and reconstruct builds the job with job.New
		// followed by BeginAttempt — neither grants a lease, and nothing
		// between them does. The discarded value is therefore always nil on
		// this path, not a lease being dropped.
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
// pol is passed straight to job.New rather than re-derived. It was job.Policy{}
// until B2.2 gave Persisted a Policy field, which silently denied a restored
// job all four permissions — it would neither verify, repair, unpack nor
// delete. It is stored resolved rather than as the upstream PP integer; see
// Persisted.Policy (ports.go) for why.
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
func reconstruct(id, name string, pol job.Policy, v job.StateView, intent job.Intent, now time.Time) (*job.Job, error) {
	j := job.New(id, name, pol)

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
