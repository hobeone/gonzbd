package sched

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// mustHoldAt puts j at `at`, holding everything that position requires, using
// the job doors and the pools DIRECTLY rather than the scheduler.
//
// Deliberately not driven through Advance. Cancel is a decision about a job's
// current holdings, and a helper that reached those holdings by running the
// scheduler would make every test here also a test of Advance — so a bug in
// Advance would show up as a cancel failure. It also lets this task land
// before Advance exists, which is what keeps each task independently
// compilable.
func mustHoldAt(t *testing.T, q *Queue, j *job.Job, at job.State) {
	t.Helper()
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	route := map[job.State][]job.State{
		job.Fetching:  {},
		job.Assessing: {job.Assessing},
		job.Repairing: {job.Assessing, job.Repairing},
	}
	steps, ok := route[at]
	if !ok {
		t.Fatalf("mustHoldAt has no route to %v; add one rather than reaching past the doors", at)
	}
	for _, to := range steps {
		if err := j.SetNext(to); err != nil {
			t.Fatalf("SetNext(%v): %v", to, err)
		}
		if err := j.Transition(to); err != nil {
			t.Fatalf("Transition(%v): %v", to, err)
		}
	}
	if needsLease(at) {
		l := q.leases.issue()
		if l == nil {
			t.Fatal("pool A had no capacity for the fixture")
		}
		if err := j.Grant(l); err != nil {
			t.Fatalf("Grant: %v", err)
		}
	}
	if needsSlot(at) && !q.slots.acquire(j.ID()) {
		t.Fatal("pool B had no capacity for the fixture")
	}
}

// TestCancel_PreBoundaryAbortsRatherThanSeizing pins §3.7's interrupt arm.
// "Immediately" describes when the worker is TOLD to stop, not when its
// resources are taken: the Manifest and StorageBarrier come with the lease, so
// reclaiming one from under a downloader mid-article is a use-after-free in
// all but name.
func TestCancel_PreBoundaryAbortsRatherThanSeizing(t *testing.T) {
	w := &stubWorkers{}
	q := New(1, 1, testClock, w)
	j := job.New("j1", "n", job.Policy{})
	mustHoldAt(t, q, j, job.Fetching)
	if err := q.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(w.aborted) != 1 || w.aborted[0] != "j1" {
		t.Errorf("aborted = %v, want [j1]", w.aborted)
	}
	if !j.HoldsLease() {
		t.Error("Cancel took the lease from a live worker; it must wait for the yield")
	}
	if j.Snapshot().State.Outcome.IsSettled() {
		t.Error("Cancel settled a job whose worker is still live")
	}
}

// TestCancel_PostBoundaryNotRunningSettlesAtOnce is scenario §5.12: a job
// restored from a restart holds nothing, so running() is false and there is no
// worker to protect. Revision 3 gated on !workDone and deadlocked here.
func TestCancel_PostBoundaryNotRunningSettlesAtOnce(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustHoldAt(t, q, j, job.Assessing)
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	l, err := j.Cross(job.Extracting) // the crossing, driven directly
	if err != nil {
		t.Fatalf("Cross: %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	// Deviation from the task-6 brief, recorded in task-6-report.md: the
	// brief's version reclaimed the lease but left the compute slot recorded
	// in q.slots, so Extracting — which needsSlot — still held everything it
	// required. That makes running() true, not false, and the job hits the
	// gate arm (IsProduction && running) rather than settling, contradicting
	// this test's own "holds nothing" comment and its assertion. A real
	// restart's Queue is a fresh process with an EMPTY slots map (pool state
	// is runtime-only, never persisted) — Cross itself never touches q.slots,
	// only the lease travels with the job — so releasing the slot directly
	// here is what actually reproduces "holds nothing," matching what
	// q.reclaim(l) already did for the lease side.
	q.slots.release(j.ID())
	// Holds nothing now, which is exactly the restored-from-restart shape.
	if err := q.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeCancelled {
		t.Errorf("Outcome = %v, want Cancelled — nothing was in flight to protect", got)
	}
}

// TestCancel_ReleasesTheComputeSlot pins the leak §3.7 names: Assessing and
// Repairing hold a slot alongside the lease, and an earlier revision reclaimed
// only the lease, leaking pool-B capacity on every cancel from those states.
//
// The job must hold the slot AT the moment Cancel runs, or this test asserts
// nothing. An earlier draft released it in the fixture — to make running()
// false so cancel would settle rather than abort — and thereby removed the
// only slot the assertion could have caught. It passed against a finishCancel
// that released nothing.
//
// SetNext is what makes the job settle-able while still holding: running() is
// `IsOpen && holds && Next == StateUnset` (§3.4), so a job whose work has
// FINISHED is not running yet still holds everything Assessing required. That
// is a real configuration — the assessor is done and the job is waiting to
// move — and it is the one where the leak bites.
func TestCancel_ReleasesTheComputeSlot(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustHoldAt(t, q, j, job.Assessing)
	if err := j.SetNext(job.Repairing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if !q.slots.holds(j.ID()) {
		t.Fatal("fixture holds no slot; this test cannot observe a slot leak")
	}
	if err := q.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if q.leases.outstanding() != 0 {
		t.Errorf("leases outstanding = %d after a cancel, want 0", q.leases.outstanding())
	}
	if q.slots.outstanding() != 0 {
		t.Errorf("slots outstanding = %d after a cancel, want 0", q.slots.outstanding())
	}
}

// The four tests below are not from the task-6 brief. check_coverage measures
// finishCancel whole-function against an 80% bar, and the brief's three tests
// (interrupt arm, post-boundary settle, slot release) never exercise the
// never-run guard, the already-settled guard, or the production+running gate
// arm, nor a reclaim failure — leaving finishCancel at 76.5%. Added to close
// that gap; see task-6-report.md for the coverage run that motivated them.

// TestCancel_NeverRunIsANoop pins finishCancel's first guard: a job with no
// attempt has State.State == StateUnset, and Outcome lives on the Attempt —
// there is none, so trying to settle it would hit ErrNoOpenAttempt. Cancel
// must return nil without trying.
func TestCancel_NeverRunIsANoop(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if j.Snapshot().State.Outcome.IsSettled() {
		t.Error("Cancel settled a job that never ran")
	}
}

// TestCancel_AlreadySettledIsANoop pins finishCancel's second guard: a job
// already closed — by a normal completion here, not by an earlier cancel —
// must not have its Outcome overwritten. job.Finish is write-once and would
// return an error if finishCancel tried anyway; the guard is what keeps
// Cancel from surfacing that error for an ordinary race with completion.
func TestCancel_AlreadySettledIsANoop(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustHoldAt(t, q, j, job.Fetching)
	l, err := j.Finish(job.OutcomeFailed, testClock())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if err := q.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeFailed {
		t.Errorf("Outcome = %v, want Failed — cancel must not overwrite an already-settled verdict", got)
	}
}

// TestCancel_ProductionRunningGates pins finishCancel's gate arm directly:
// IsProduction(state) && running() must return nil without aborting a worker
// (there is nothing to abort past the boundary — Production has no interrupt
// arm) and without settling the job — D-I11 lets an in-flight Production
// attempt reach its own end instead.
func TestCancel_ProductionRunningGates(t *testing.T) {
	w := &stubWorkers{}
	q := New(1, 1, testClock, w)
	j := job.New("j1", "n", job.Policy{})
	mustHoldAt(t, q, j, job.Assessing)
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	l, err := j.Cross(job.Extracting)
	if err != nil {
		t.Fatalf("Cross: %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	// Extracting still needs a compute slot, and nothing released it: the job
	// holds everything Extracting requires and Next is unset, so running() is
	// true and IsProduction(Extracting) is true — the gate arm.
	if err := q.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(w.aborted) != 0 {
		t.Errorf("aborted = %v, want none — Production has no interrupt arm", w.aborted)
	}
	if j.Snapshot().State.Outcome.IsSettled() {
		t.Error("Cancel settled a job still running in Production; the gate must let it finish")
	}
}

// TestCancel_SettledThenCancelledReleasesTheSlot pins the final whole-branch
// review's Critical 1: cancelling a job that has ALREADY settled must release
// the slot its position still holds. Before this fix, finishCancel's settled
// arm returned bare, and Advance routes s.Intent == IntentCancel to
// finishCancel before it ever reaches Advance's own settled-branch release —
// so once IntentCancel latches on a settled job, no later Advance tick can
// recover the slot either. Both real sequences that reach this — a worker
// failing at a correctness state and the user then cancelling, or a user
// cancelling a running Production job whose worker then completes with
// OutcomeOK — settle first and are cancelled second, which is exactly what
// mustHoldAt + Finish + Cancel reproduces here.
func TestCancel_SettledThenCancelledReleasesTheSlot(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustHoldAt(t, q, j, job.Assessing)
	if !q.slots.holds(j.ID()) {
		t.Fatal("fixture holds no slot; this test cannot observe the leak")
	}
	l, err := j.Finish(job.OutcomeFailed, testClock())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if err := q.Cancel(j); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if q.slots.holds(j.ID()) {
		t.Error("slot still held after cancelling an already-settled job; " +
			"finishCancel's settled arm must release it")
	}
	// Further Advance ticks must not be needed to recover it — and, per the
	// routing this test exists to pin, cannot: Advance sends an IntentCancel
	// job straight to finishCancel, never reaching its own settled branch.
	for i := range 5 {
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance %d: %v", i, err)
		}
	}
	if q.slots.holds(j.ID()) {
		t.Errorf("slot still held after 5 further Advance ticks; outstanding=%d", q.slots.outstanding())
	}
}

// TestFinishCancel_FailedFinishDoesNotReleaseTheSlot pins B3: if Finish
// itself refuses (here, a write-once violation — the attempt already
// settled), the attempt was NOT cancelled, so finishCancel must not release
// the slot its still-occupied position requires, and must return the error
// rather than let it vanish behind an unconditional release.
//
// Constructed with a snapshot that is stale on purpose, exactly the shape
// finishCancel's own doc comment names as the reason it takes its caller's
// already-read snapshot rather than re-reading: s is captured while the job's
// work has finished but before it moves (Next = Repairing, so running(s) is
// false and finishCancel takes the settle path), and then the job is
// ACTUALLY settled — Failed, by a real worker completing independently of
// the snapshot — before finishCancel ever runs. finishCancel's own
// Finish(OutcomeCancelled) then hits the write-once guard
// (internal/job/attempt.go's `if a.outcome.IsSettled()`), which is exactly
// what a genuine race between a settling worker and a concurrent Cancel
// would produce; the stale snapshot reproduces that race deterministically
// without needing real goroutines.
func TestFinishCancel_FailedFinishDoesNotReleaseTheSlot(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustHoldAt(t, q, j, job.Assessing)
	if err := j.SetNext(job.Repairing); err != nil { // work finished; not yet moved
		t.Fatalf("SetNext: %v", err)
	}
	s := j.Snapshot() // stale on purpose — see comment above

	l, err := j.Finish(job.OutcomeFailed, testClock()) // the real, independent settle
	if err != nil {
		t.Fatalf("fixture Finish: %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !q.slots.holds(j.ID()) {
		t.Fatal("fixture holds no slot; this test cannot observe a wrongful release")
	}

	if err := q.finishCancel(j, s); err == nil {
		t.Fatal("finishCancel = nil, want an error — Finish must refuse a second settle")
	}
	if !q.slots.holds(j.ID()) {
		t.Error("finishCancel released the slot despite Finish failing; the attempt was NOT cancelled")
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeFailed {
		t.Errorf("Outcome = %v, want Failed — finishCancel's failed Finish must not have overwritten the real verdict", got)
	}
}

// TestFinishCancel_ReclaimErrorPropagates pins the ordering the plan review
// flagged: the slot must be freed even when reclaim's identity audit fails,
// and the audit's error must still reach the caller. A lease Grant-ed to the
// job but never issued by q's own pool A makes j.Finish yield a lease that
// q.reclaim rejects with errNotOutstanding.
func TestFinishCancel_ReclaimErrorPropagates(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	foreign := New(1, 0, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if err := j.SetNext(job.Assessing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := j.Transition(job.Assessing); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	l := foreign.leases.issue()
	if l == nil {
		t.Fatal("could not issue a lease from the foreign queue's pool")
	}
	if err := j.Grant(l); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !q.slots.acquire(j.ID()) {
		t.Fatal("pool B had no capacity for the fixture")
	}
	// Next set: work at Assessing has ended, so running() is false and
	// finishCancel takes the settle path rather than the running gate/abort.
	if err := j.SetNext(job.Repairing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	// Called directly rather than through Cancel: finishCancel takes the
	// snapshot its caller already read (see its doc comment) rather than
	// re-reading it, so a direct call with a freshly taken snapshot exercises
	// exactly the contract Cancel relies on.
	if err := q.finishCancel(j, j.Snapshot()); !errors.Is(err, errNotOutstanding) {
		t.Errorf("finishCancel = %v, want errNotOutstanding — the lease was never q's own", err)
	}
	if q.slots.outstanding() != 0 {
		t.Errorf("slots outstanding = %d after a failed reclaim, want 0 — "+
			"the slot must be freed even when the audit rejects the lease", q.slots.outstanding())
	}
}
