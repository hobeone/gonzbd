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
// The launched claim is cleared by a deferred call, covering every return
// path, not one written after Settle's success line. A call to Finished
// happens only after the Runner's Run invocation has already returned — that
// is what a worker calling Finished means — so the claim it made in launch is
// stale the moment Finished is entered, whether or not Settle goes on to
// succeed. Clearing only on success would leave launched[id] set forever
// after a Settle failure (a refused Finish, a failed reclaim identity audit)
// or the OutcomeCancelled rejection above: the job would then be permanently
// unlaunchable, since claimLaunched never returns true for an ID already set.
// Clearing unconditionally BEFORE calling Settle, instead of deferring, would
// be wrong the other way: a concurrent tick's launch reads Render(j).Running
// from Queue state, which Settle has not yet changed, so it could observe the
// job still Running with the claim already clear and start a second worker on
// resources the first has not yet released. The defer is what makes the clear
// land after Settle has run — so Running (if it changes) has already changed
// — while still being unconditional on Settle's outcome.
func (d *Dispatcher) Finished(j *job.Job, o job.Outcome) error {
	if o == job.OutcomeCancelled {
		d.clearLaunched(j.ID())
		return fmt.Errorf("dispatch: Finished(%s): OutcomeCancelled is reserved for the cancel latch", j.ID())
	}
	err := d.q.Settle(j, o)
	// Cleared AFTER Settle and BEFORE kick, and unconditional on Settle's
	// outcome. A defer satisfied the first and third and broke the second:
	// kick ran first, so the woken tick could reach launch() while the claim
	// was still held, find claimLaunched false, decline to start a worker —
	// and consume the wake doing it, leaving the job holding resources with
	// nobody working it until the next timer tick.
	d.clearLaunched(j.ID())
	if err != nil {
		return fmt.Errorf("dispatch: Finished(%s): %w", j.ID(), err)
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
// The launched claim is cleared by a deferred call, for the same reason as
// Finished: a call to Yielded means the Runner's Run invocation has already
// returned, so the claim is stale on entry regardless of whether Park
// succeeds. Clearing only after a successful Park would strand the claim
// forever on a Park failure, making the job permanently unlaunchable;
// clearing before calling Park would let a concurrent tick's launch observe
// the job still Running (Park has not yet released it) with the claim
// already free, and start a second worker on resources the first has not yet
// surrendered.
func (d *Dispatcher) Yielded(j *job.Job) error {
	err := d.q.Park(j)
	// After Park, before kick — see Finished above for why the ordering is
	// load-bearing in both directions.
	d.clearLaunched(j.ID())
	if err != nil {
		return fmt.Errorf("dispatch: Yielded(%s): %w", j.ID(), err)
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
func (d *Dispatcher) launch(ctx context.Context, j *job.Job) {
	v := d.q.Render(j)
	if !v.Running || v.Intent != job.IntentRun {
		return
	}
	if d.claimLaunched(j.ID()) {
		d.runner.Run(ctx, j.ID(), v.State)
	}
}

// claimLaunched sets launched[id] under d.mu and reports whether this call was
// the one that set it, so a later tick does not start a second worker for a
// job already being worked. Finished, Yielded and Stop's sweep are its three
// exit-path clearers — `grep -n 'd\.clearLaunched(' internal/dispatch/*.go |
// grep -v _test.go` finds three lines, one per site.
func (d *Dispatcher) claimLaunched(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.launched[id] {
		return false
	}
	d.launched[id] = true
	return true
}

func (d *Dispatcher) clearLaunched(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.launched, id)
}
