package dispatch

import (
	"context"
	"errors"
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

// Yielded records a worker exiting without finishing its state's work by job
// ID alone, delegating to YieldedFor with a nil expected job pointer.
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

// YieldedFor records a worker exiting without finishing its state's work: a pause
// yield at an article boundary, an Abort, a shutdown, a dead connection.
//
// When expected is non-nil, it asserts that the currently registered job object
// matches expected by pointer identity; if the job was removed or replaced by
// another attempt under the same ID, it no-ops and returns ErrNotFound.
//
// When registered (and matching expected when non-nil), it parks unconditionally.
// Park is total — slot release is a map delete, Surrender returns nil when
// nothing is held, and reclaim no-ops on nil — so the dispatcher never has to
// decide whether a yielding worker still holds something. That totality is what
// makes one door correct for every exit shape.
//
// This is the input the tick cannot compute. Advance branch 2 returns early
// while holds() is true, because the Queue cannot distinguish a working holder
// from a yielded one, and stripping a live worker is the worse failure. Only
// the dispatcher knows which it is.
//
// The launched claim latch is cleared via clearLaunched(id),
// for the same reason as Finished: for runner workers, YieldedFor is called after
// Run returns or when yielding mid-pipeline, so the claim is stale on entry
// regardless of whether Park succeeds. External callers call it explicitly to
// release the launch claim without waiting for goroutine teardown.
// Clearing only after a successful Park would strand the claim forever on a
// Park failure, making the job permanently unlaunchable; clearing before
// calling Park would let a concurrent tick's launch observe the job still
// Running (Park has not yet released it) with the claim already free, and start
// a second worker on resources the first has not yet surrendered.
func (d *Dispatcher) YieldedFor(id string, expected *job.Job) error {
	j, ok := d.lookupFor(id, expected)
	if !ok {
		return fmt.Errorf("dispatch: Yielded: no job %q: %w", id, ErrNotFound)
	}

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

// ErrStaleReport is AdvanceFrom's and YieldedFrom's refusal of a report about a state the job is
// no longer working: it has moved on, settled, or already recorded where it
// goes next.
var ErrStaleReport = errors.New("dispatch: report is for a state the job is not working")

// AdvanceFrom records that j's worker finished the work of state from and that
// the job continues to next. It is the exit report for finished work; Yielded
// is the one for work that stopped unfinished.
//
// The verdict, the park and the release of the launch claim happen in one
// sched.Queue.Handoff span, which is what makes the report atomic against the
// tick. Recording next and then yielding as two calls let a tick land between
// them: it moved the job to next and launched that state's worker, and the
// yield that followed parked that worker's resources and cleared its claim, so
// the next tick launched the state a second time.
//
// It changes nothing and returns ErrStaleReport unless the job is still open
// at from with no next recorded, so a late or repeated report cannot touch the
// state that replaced from. It returns ErrNotFound when j is not the instance
// registered under its ID. A verdict SetNext refuses settles the job
// OutcomeFailed (sched.Queue.Handoff); next == StateUnset is refused before
// anything is touched, since YieldedFrom is the door for a report with no
// verdict.
func (d *Dispatcher) AdvanceFrom(j *job.Job, from, next job.State) error {
	if next == job.StateUnset {
		return fmt.Errorf("dispatch: AdvanceFrom: %w: StateUnset is not a verdict", job.ErrIllegalTransition)
	}
	return d.handoff("AdvanceFrom", j, from, next)
}

// YieldedFrom records that j's worker for state from stopped without finishing
// its work. It is Yielded scoped to one state: it parks the job and clears the
// claim only while the job is still open at from with no next recorded, and
// otherwise returns ErrStaleReport and leaves alone whatever worker the job has
// now. It returns ErrNotFound when j is not the instance registered under its
// ID.
func (d *Dispatcher) YieldedFrom(j *job.Job, from job.State) error {
	return d.handoff("YieldedFrom", j, from, job.StateUnset)
}

// handoff is AdvanceFrom's and YieldedFrom's body: the instance check, then
// one sched.Queue.Handoff span that records next (if any), parks, and clears
// the launch claim.
//
// The claim is cleared inside that span, which takes d.mu under Queue.mu. That
// keeps the lock rule, because d.mu is never held across a call into sched.
// This is defence in depth rather than what stops a double launch today: while
// the claim taken for from is held, launch's claimLaunched refuses next's
// worker until the clear anyway. It matters when no claim for from is held at
// the clear — one never taken, as after a stall's resume or at startup, or one
// another by-ID clearer released between the span and a later clear — because
// a tick could then launch next and have its claim dropped by the late clear.
// Inside the span the job cannot leave from, because Advance needs Queue.mu to
// move it. No test pins the placement: no seam can interleave a tick there.
func (d *Dispatcher) handoff(door string, j *job.Job, from, next job.State) error {
	if j == nil {
		return fmt.Errorf("dispatch: %s: nil job: %w", door, ErrNotFound)
	}
	id := j.ID()
	if _, ok := d.lookupFor(id, j); !ok {
		return fmt.Errorf("dispatch: %s: no job %q: %w", door, id, ErrNotFound)
	}
	handed, err := d.q.Handoff(j, from, next, func() { d.clearLaunchedFor(j) })
	if !handed {
		return fmt.Errorf("dispatch: %s(%s, %s -> %s): %w", door, id, from, next, ErrStaleReport)
	}
	d.kick()
	if err != nil {
		return fmt.Errorf("dispatch: %s(%s, %s -> %s): %w", door, id, from, next, err)
	}
	return nil
}

// launch starts a worker if the job is runnable and still wanted.
//
// It re-reads the snapshot rather than trusting the one the tick took: between
// Advance granting resources and this call, the manifest read ran unlocked
// (D-B8) and a concurrent Cancel may have latched IntentCancel. Launching
// anyway is not a correctness failure — the next tick aborts it — but it starts
// work the user already cancelled and pays a further tick to stop it.
//
// The Running check is repeated after the claim, and only the second one
// decides. An exit report (Finished, Yielded, AdvanceFrom or YieldedFrom) that lands
// between the first check and the claim moves the job and clears a claim that
// does not exist yet; a claim taken after it has no report left to clear it,
// so the job is not launched again until a removal or Stop clears it. Checked
// after the claim, that report is visible, and any report after the claim
// clears it. That rests on two branches:
//   - a report changes Render before it clears the claim: Finished's Settle
//     closes the attempt, YieldedFor's Park drops the lease or slot, and
//     AdvanceFrom's and YieldedFrom's Handoff parks, each ahead of
//     clearLaunched, so the re-check reads Running false;
//   - nothing can re-grant the job between that report and the re-check,
//     because Advance and launch both run only from tick (tick.go), which
//     never overlaps itself.
//
// The first check only keeps a tick from taking and dropping a claim for
// every job that is not running.
//
// A job that reads as running holds what Advance granted it, and every path
// that declines to launch it gives that back through parkUnlaunched: sched
// cannot tell a granted job from a working one, so a job that holds with no
// worker is never parked by a later Advance, and the worker that would
// report for it was never started.
func (d *Dispatcher) launch(j *job.Job) {
	v := d.q.Render(j)
	if !v.Running {
		return
	}
	if v.Intent != job.IntentRun {
		d.parkUnlaunched(j)
		return
	}
	if d.beforeClaim != nil {
		d.beforeClaim(j.ID())
	}
	if !d.claimLaunched(j.ID()) {
		d.parkUnlaunched(j)
		return
	}
	v = d.q.Render(j)
	if !v.Running || v.Intent != job.IntentRun {
		// This call holds the claim, so no worker exists. Parked before the
		// claim is cleared, in YieldedFor's order.
		if v.Running {
			d.parkGrant(j)
		}
		d.clearLaunched(j.ID())
		return
	}
	d.mu.Lock()
	runCtx := d.ctx
	d.mu.Unlock()
	d.runner.Run(runCtx, j.ID(), v.State)
}

// parkUnlaunched gives back what Advance granted a job that launch is not
// starting, unless a worker holds the job's launch claim and so its
// resources.
//
// A claim absent here stays absent until the park: the only claimLaunched
// call is in launch, and launch runs only from tick (`git grep -n
// 'd\.launch(' -- 'internal/dispatch/*.go' ':!*_test.go'` finds 1 line),
// which never overlaps itself. The claim is read before Render, because an
// exit report parks before it clears the claim: a claim already cleared by
// one means Render sees that park.
func (d *Dispatcher) parkUnlaunched(j *job.Job) {
	d.mu.Lock()
	_, claimed := d.launched[j.ID()]
	d.mu.Unlock()
	if claimed || !d.q.Render(j).Running {
		return
	}
	d.parkGrant(j)
}

// parkGrant parks a job that holds resources with no worker.
func (d *Dispatcher) parkGrant(j *job.Job) {
	if err := d.q.Park(j); err != nil {
		d.log.Error("failed to return the resources of a job that was not launched",
			"job", j.ID(), "err", err)
	}
}

// claimLaunched sets launched[id] under d.mu and reports whether this call was
// the one that set it, so a later tick does not start a second worker for a
// job already being worked. Finished, YieldedFor, clearLaunchedFor (for
// AdvanceFrom and YieldedFrom), Stop's sweep and deregister are its five
// exit-path clearers, and launch clears a claim it took
// for a job that stopped running before the claim — `grep -n 'd\.clearLaunched(' internal/dispatch/*.go |
// grep -v _test.go` finds six lines, one per site.
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

// clearLaunchedFor clears the launch claim under j's ID only while j is the
// instance registered there. A deregistered instance's claim went with it, and
// the ID may by then carry a later instance's claim.
//
// The lookup and the clear are two d.mu spans. handoff calls this inside
// Handoff's Queue.mu span. A later instance registered between the two spans
// would need Advance, and so Queue.mu, to become Running, so launch cannot
// take its claim before the clear.
func (d *Dispatcher) clearLaunchedFor(j *job.Job) {
	if _, ok := d.lookupFor(j.ID(), j); ok {
		d.clearLaunched(j.ID())
	}
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
// to Finished, Yielded, AdvanceFrom or YieldedFrom). Returns nil immediately if no worker
// is launched.
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
