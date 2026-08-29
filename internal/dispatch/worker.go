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
func (d *Dispatcher) Finished(j *job.Job, o job.Outcome) error {
	if o == job.OutcomeCancelled {
		return fmt.Errorf("dispatch: Finished(%s): OutcomeCancelled is reserved for the cancel latch", j.ID())
	}
	if err := d.q.Settle(j, o); err != nil {
		return fmt.Errorf("dispatch: Finished(%s): %w", j.ID(), err)
	}
	d.clearLaunched(j.ID())
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
func (d *Dispatcher) Yielded(j *job.Job) error {
	if err := d.q.Park(j); err != nil {
		return fmt.Errorf("dispatch: Yielded(%s): %w", j.ID(), err)
	}
	d.clearLaunched(j.ID())
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
// job already being worked. Finished and Yielded clear it.
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
