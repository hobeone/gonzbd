package dispatch

import (
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
	started   bool //nolint:unused // read and written by Start/Stop, added in Task 3

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
