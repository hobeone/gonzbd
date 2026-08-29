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
// Task 2 declares every field the dispatcher will ever hold, including ones no
// method in this package reads yet (res, store, runner, tickEvery, stop, done,
// started, log). Go permits unused struct fields, and the alternative — adding
// fields piecemeal per task — would leave this file's own test constructor
// unable to compile until the last task landed. Tasks 3-7 give each field its
// first reader; declaring it here is not implementing it.
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
// It panics on a nil Residency, Store or Workers for the same reason
// sched.New panics on a nil Workers: these are construction-time programmer
// errors, not state an earlier build wrote, so Standing Design Rule 1's
// guard-removal argument does not apply. Failing here beats a nil dereference
// on the ticker goroutine with no construction frame left to explain it.
func New(leaseCap, slotCap int, tickEvery time.Duration, clock func() time.Time, w sched.Workers, r Residency, s Store) *Dispatcher {
	if w == nil {
		panic("dispatch: New: Workers must not be nil")
	}
	if r == nil {
		panic("dispatch: New: Residency must not be nil")
	}
	if s == nil {
		panic("dispatch: New: Store must not be nil")
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
	}
	return firstErr
}

// restore is implemented in Task 7. Until then it reads the store and reports
// any error, so Start's error path is real rather than dead code.
func (d *Dispatcher) restore(ctx context.Context) error {
	_, err := d.store.Load(ctx)
	return err
}
