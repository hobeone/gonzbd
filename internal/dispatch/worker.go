package dispatch

import (
	"context"
	"fmt"

	"github.com/hobeone/gonzbd/internal/job"
)

// Finished records a worker's terminal completion.
//
// It rejects OutcomeCancelled before touching the Queue. sched.Settle refuses
// it too, but failing here names the caller: only the cancel latch may produce
// Cancelled, and a worker allowed to report it could make any exit look like a
// user deletion.
//
// The launched claim latch is cleared by clearLaunched, covering every return
// path, not one written after Settle's success line. For runner workers,
// Finished is called by the worker goroutine after Run returns, so the claim
// it made in launch is stale the moment Finished is entered, whether or not
// Settle goes on to succeed. External callers call it explicitly to release the
// launch claim without waiting for goroutine teardown.
// Clearing only on success would leave launched[id] set forever
// after a Settle failure (a refused Finish, a failed reclaim identity audit)
// or the OutcomeCancelled rejection above: the job would then be permanently
// unlaunchable, since claimLaunched never returns true for an ID already set.
// Clearing unconditionally BEFORE calling Settle would be wrong the other way:
// a concurrent tick's launch reads Render(j).Running from Queue state, which
// Settle has not yet changed, so it could observe the job still Running with
// the claim already clear and start a second worker on resources the first has
// not yet released. Clearing after Settle has run ensures Running (if it
// changes) has already changed, while still being unconditional on Settle's
// outcome.
func (d *Dispatcher) Finished(id string, o job.Outcome) error {
	j, ok := d.lookup(id)
	if !ok {
		return fmt.Errorf("dispatch: Finished: no job %q: %w", id, ErrNotFound)
	}
	// One clearLaunched call, on every path: the rejection below and a failed
	// Settle both still have to release the claim, and a second call site
	// would be a second thing to forget. claimLaunched's citation counts
	// these, one per exit door.
	var err error
	if o == job.OutcomeCancelled {
		err = fmt.Errorf("dispatch: Finished(%s): OutcomeCancelled is reserved for the cancel latch", id)
	} else if serr := d.q.Settle(j, o); serr != nil {
		err = fmt.Errorf("dispatch: Finished(%s): %w", id, serr)
	}
	// Cleared AFTER Settle and BEFORE kick, and unconditional on Settle's
	// outcome. A defer satisfied the first and third and broke the second:
	// kick ran first, so the woken tick could reach launch() while the claim
	// was still held, find claimLaunched false, decline to start a worker —
	// and consume the wake doing it, leaving the job holding resources with
	// nobody working it until the next timer tick.
	d.clearLaunched(id)
	if err != nil {
		return err
	}
	d.kick()
	return nil
}

// Yielded records a worker exiting without finishing its state's work: a pause
// yield at an article boundary, an Abort, a shutdown, a dead connection.
//
// It parks unconditionally. Park is total — slot release is a map delete,
// Surrender returns nil when nothing is held, and reclaim no-ops on nil — so
// the dispatcher never has to decide whether a yielding worker still holds
// something. That totality is what makes one door correct for every exit shape.
//
// This is the input the tick cannot compute. Advance branch 2 returns early
// while holds() is true, because the Queue cannot distinguish a working holder
// from a yielded one, and stripping a live worker is the worse failure. Only
// the dispatcher knows which it is.
//
// The launched claim latch is cleared via clearLaunched(id),
// for the same reason as Finished: for runner workers, Yielded is called after
// Run returns or when yielding mid-pipeline, so the claim is stale on entry
// regardless of whether Park succeeds. External callers call it explicitly to
// release the launch claim without waiting for goroutine teardown.
// Clearing only after a successful Park would strand the claim forever on a
// Park failure, making the job permanently unlaunchable; clearing before
// calling Park would let a concurrent tick's launch observe the job still
// Running (Park has not yet released it) with the claim already free, and start
// a second worker on resources the first has not yet surrendered.
func (d *Dispatcher) Yielded(id string) error {
	return d.YieldedFor(id, nil)
}

// YieldedJob records a worker exiting without finishing its state's work.
// If the job registered under j.ID() is no longer the instance j (e.g. because
// the aborted job was removed and a new attempt with the same ID was registered),
// YieldedJob no-ops and returns ErrNotFound so it does not clear the new
// attempt's launch claim or park its resources.
func (d *Dispatcher) YieldedJob(j *job.Job) error {
	if j == nil {
		return fmt.Errorf("dispatch: YieldedJob: nil job: %w", ErrNotFound)
	}
	return d.YieldedFor(j.ID(), j)
}

// YieldedFor records a worker exiting without finishing its state's work.
// When expected is non-nil, it asserts that the currently registered job object
// matches expected by pointer identity; if the job was removed or replaced by
// another attempt under the same ID, it no-ops and returns ErrNotFound.
func (d *Dispatcher) YieldedFor(id string, expected *job.Job) error {
	d.mu.Lock()
	e, ok := d.byID[id]
	if !ok || (expected != nil && e.j != expected) {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: Yielded: no job %q: %w", id, ErrNotFound)
	}
	j := e.j
	d.mu.Unlock()

	err := d.q.Park(j)
	// After Park, before kick — see Finished above for why the ordering is
	// load-bearing in both directions.
	d.clearLaunched(id)
	if err != nil {
		return fmt.Errorf("dispatch: Yielded(%s): %w", id, err)
	}
	d.kick()
	return nil
}

// launch starts a worker if the job is runnable and still wanted.
//
// It re-reads the snapshot rather than trusting the one the tick took: between
// Advance granting resources and this call, the manifest read ran unlocked
// (D-B8) and a concurrent Cancel may have latched IntentCancel. Launching
// anyway is not a correctness failure — the next tick aborts it — but it starts
// work the user already cancelled and pays a further tick to stop it.
func (d *Dispatcher) launch(j *job.Job) {
	v := d.q.Render(j)
	if !v.Running || v.Intent != job.IntentRun {
		return
	}
	if d.claimLaunched(j.ID()) {
		d.mu.Lock()
		runCtx := d.ctx
		d.mu.Unlock()
		d.runner.Run(runCtx, j.ID(), v.State)
	}
}

// claimLaunched sets launched[id] under d.mu and reports whether this call was
// the one that set it, so a later tick does not start a second worker for a
// job already being worked. Finished, Yielded, Stop's sweep and deregister are its four
// exit-path clearers — `grep -n 'd\.clearLaunched(' internal/dispatch/*.go |
// grep -v _test.go` finds four lines, one per site.
func (d *Dispatcher) claimLaunched(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.admitsLocked(id) {
		return false
	}
	if _, ok := d.launched[id]; ok {
		return false
	}
	d.launched[id] = make(chan struct{})
	return true
}

func (d *Dispatcher) clearLaunched(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ch, ok := d.launched[id]; ok {
		close(ch)
		delete(d.launched, id)
	}
}

// waitLaunched waits for the job's launch claim latch to be cleared (by a call
// to Finished or Yielded). Returns nil immediately if no worker is launched.
func (d *Dispatcher) waitLaunched(ctx context.Context, id string) error {
	d.mu.Lock()
	ch := d.launched[id]
	d.mu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	default:
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
