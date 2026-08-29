package sched

import (
	"errors"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// mustAdvanceTo drives j through the real doors until it sits at want, using
// the Queue rather than constructing state, so a configuration this helper
// cannot reach is one the machine cannot reach.
//
// The scheduler-driven counterpart to Task 6's mustHoldAt, which reaches the
// same positions through the job doors and the pools directly. Both exist on
// purpose: cancel is a decision about holdings and must not depend on Advance,
// while the scenarios in Task 8 are about the scheduler and should go through it.
func mustAdvanceTo(t *testing.T, q *Queue, j *job.Job, want job.State) {
	t.Helper()
	if err := q.Advance(j); err != nil { // branch 1: opens the attempt
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil { // branch 2: grants for Fetching
		t.Fatalf("Advance (grant): %v", err)
	}
	for j.Snapshot().State.State != want {
		from := j.Snapshot().State.State
		next := map[job.State]job.State{job.Fetching: job.Assessing, job.Assessing: job.Repairing}[from]
		if next == job.StateUnset {
			t.Fatalf("mustAdvanceTo has no route from %v to %v", from, want)
		}
		if err := j.SetNext(next); err != nil {
			t.Fatalf("SetNext(%v): %v", next, err)
		}
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (%v → %v): %v", from, next, err)
		}
	}
}

func mustAdvanceToSettled(t *testing.T, q *Queue, j *job.Job, o job.Outcome) {
	t.Helper()
	mustAdvanceTo(t, q, j, job.Fetching)
	l, err := j.Finish(o, testClock())
	if err != nil {
		t.Fatalf("Finish(%v): %v", o, err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
}

// TestAdvance_BranchOne_StartsANeverRunJob covers §3.6 branch 1. No lease is
// taken here — D-I12 decoupled opening an attempt from holding one, so a retry
// can never fail for want of capacity.
func TestAdvance_BranchOne_StartsANeverRunJob(t *testing.T) {
	q := New(0, 0, testClock, &stubWorkers{}) // no capacity at all
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !j.HasRun() {
		t.Error("a never-run job was not started; BeginAttempt needs no lease (D-I12)")
	}
}

// TestAdvance_BranchOne_GatedNeverRunJobStaysUnstarted covers §3.6 branch 1's
// gate check: a paused job must not be started even though BeginAttempt takes
// no resources. Without the gate check, a user pausing a never-run job would
// find it running anyway.
func TestAdvance_BranchOne_GatedNeverRunJobStaysUnstarted(t *testing.T) {
	q := New(0, 0, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if j.HasRun() {
		t.Error("a paused, never-run job was started; branch 1 must gate before BeginAttempt")
	}
}

// TestAdvance_SettledJobDoesNotOpenANewAttempt asserts j.Attempts() is
// unchanged after Advance on a settled job. It does NOT discriminate §3.6's
// settled early return: neutering that early return (`if false &&
// s.State.Outcome.IsSettled()`) still reports this test `ok`. Traced why —
// with the early return removed, a settled job (State != StateUnset, so
// branch 1 can never run regardless) falls into branch 2, where holds()
// is false (no lease held) and grantFor is called; grantFor issues a lease
// from the pool, calls j.Grant, which refuses because the attempt has no
// open door (job.Grant's own settled-attempt guard), and the refused lease
// is reclaimed within the same call. Attempts is untouched either way,
// because only branch 1 — gated on s.State.State == StateUnset, which a
// settled job's State never is — calls BeginAttempt at all. No fixture
// reachable from this test's vantage (an Advance call on an already-settled
// job) can make Attempts differ between the two versions, so this assertion
// is a structural invariant of State != StateUnset, not a pin on the settled
// early return.
//
// The early return's own observable effect — the slot it releases — is what
// actually distinguishes it, and TestAdvance_SettledJobReleasesItsSlot below
// pins that: neutering the same condition turns it red.
func TestAdvance_SettledJobDoesNotOpenANewAttempt(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceToSettled(t, q, j, job.OutcomeFailed)
	before := j.Attempts()
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if j.Attempts() != before {
		t.Errorf("Advance opened attempt %d on a settled job; retry is q.Retry's job", j.Attempts())
	}
}

// TestAdvance_SettledJobReleasesItsSlot pins the settled-early-return's
// release. A job settled at Assessing keeps its position (§3.3) but must not
// hold pool-B capacity forever; nothing else on the settled path frees it.
func TestAdvance_SettledJobReleasesItsSlot(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if !q.slots.holds(j.ID()) {
		t.Fatal("fixture holds no slot at Assessing; this test cannot observe the release")
	}
	l, err := j.Finish(job.OutcomeFailed, testClock())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if q.slots.holds(j.ID()) {
		t.Error("still holds a compute slot after settling; the settled branch must release it")
	}
}

// TestAdvance_BranchThree_CrossingReclaimsTheLease covers §3.6 branch 3 and is
// the case §3.9's table calls out: the lease must come back at the crossing,
// and grantFor's result is deliberately ignored because the decision was
// already recorded in next.
//
// Departs from the brief's literal fixture, `New(1, 0, ...) // one lease, NO
// slots` followed directly by `mustAdvanceTo(t, q, j, job.Assessing)`. That
// premise cannot be reached: Assessing needsSlot, and branch 3's ordinary
// (non-crossing) move grants for the destination BEFORE calling Transition,
// so with zero slot capacity grantFor always fails on the slot check and
// Advance returns nil without ever leaving Fetching — mustAdvanceTo's loop
// then never terminates. Confirmed empirically: a bounded probe
// (q.Advance(j) called 5 times in a row after SetNext(Assessing), capacity
// New(1, 0, ...)) logged `state=Fetching` on every one of the 5 iterations;
// two unmodified `go test -run TestAdvance` runs against the brief's literal
// fixture also hung past a 3+ minute wall-clock timeout with zero progress.
//
// The fix reaches Assessing with real slot capacity (needed to get there at
// all), then makes the slot genuinely unavailable at the crossing itself,
// which is the one instant the "NO slots" premise can actually apply.
// Leaving j's own Assessing slot in place would not do this: slotPool.acquire
// is idempotent for a job that already holds one (pool.go), so grantFor's
// slot check at the crossing would succeed trivially regardless of capacity
// and the ignored-result branch would go unexercised. Releasing j's slot and
// zeroing capacity directly is the same technique
// TestCancel_PostBoundaryNotRunningSettlesAtOnce (cancel_test.go) uses, for
// the same reason: simulating a resource the job no longer holds, in a
// package-internal test that may reach into the pool directly.
func TestAdvance_BranchThree_CrossingReclaimsTheLease(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{}) // one lease, one slot — enough to reach Assessing
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	q.slots.release(j.ID())
	q.slots.capacity = 0 // pool B has no capacity at the crossing
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Extracting {
		t.Errorf("State = %v, want Extracting — crossing does not wait for a slot", got)
	}
	if q.leases.outstanding() != 0 {
		t.Errorf("leases outstanding = %d after a crossing, want 0", q.leases.outstanding())
	}
}

// TestAdvance_ParkingAGatedJobReturnsItsLease is §3.8's deadlock, pinned. A
// `return nil` that merely declines to move leaves a paused job holding a
// pool-A lease forever.
func TestAdvance_ParkingAGatedJobReturnsItsLease(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if err := j.SetNext(job.Fetching); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if j.HoldsLease() {
		t.Error("a gated job still holds its lease; §3.8 calls that a deadlock")
	}
	if q.leases.outstanding() != 0 {
		t.Errorf("leases outstanding = %d after parking, want 0", q.leases.outstanding())
	}
	// Pool B is the half §3.8's argument never mentions, and the half an
	// earlier draft leaked: Assessing holds a slot, and parking returned only
	// the lease. A paused job occupying a compute slot is the same deadlock
	// wearing the other pool's name.
	if q.slots.outstanding() != 0 {
		t.Errorf("slots outstanding = %d after parking, want 0", q.slots.outstanding())
	}
}

// TestAdvance_ParkingAGatedRunnableJobAlsoReturnsItsResources covers branch
// 3's park arm (§3.6): work at Assessing has FINISHED (SetNext(Repairing) is
// called before pausing) and the job is gated, so it must park rather than
// move. TestAdvance_ParkingAGatedJobReturnsItsLease above is NOT the branch-2
// contrast this comment once claimed: it also calls SetNext (to Fetching)
// before pausing, so it too has s.State.Next != StateUnset and lands on
// branch 3 — both tests exercise the same park arm, differing only in which
// destination state was pending (a demotion out of the correctness loop
// there, a move within it here). Branch 2's OWN park arm — s.State.Next ==
// StateUnset, work still unfinished — has no fixture in this file's earlier
// tests at all; it is pinned separately by
// TestAdvance_BranchTwo_ParkingAGatedNotYetRunningJobReturnsItsLease below.
func TestAdvance_ParkingAGatedRunnableJobAlsoReturnsItsResources(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if err := j.SetNext(job.Repairing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if j.HoldsLease() {
		t.Error("a gated job with finished work still holds its lease")
	}
	if q.slots.outstanding() != 0 {
		t.Errorf("slots outstanding = %d after parking a finished-work job, want 0", q.slots.outstanding())
	}
	if got := j.Snapshot().State.State; got != job.Assessing {
		t.Errorf("State = %v, want Assessing — a gated job must not move", got)
	}
}

// TestAdvance_OrderGatedPrecedesTheCrossing is branch 3's other overlap, and
// the highest-stakes pair in the file: a job at Assessing with Next =
// Extracting (a legal SetNext — Assessing → Extracting is the one byCross
// edge) and IntentPause set satisfies BOTH the gate check and the
// IsCorrectness && IsProduction crossing check at once. §3.8 says a paused
// job must park, never cross — the boundary is irreversible (D3: crossing
// deletes archives, moves files, runs user scripts) and a pause is exactly
// the signal that the user does not want that to happen yet.
//
// TestAdvance_ParkingAGatedRunnableJobAlsoReturnsItsResources covers branch
// 3's park arm generally, but with Next = Repairing, which is not the
// crossing edge — it never reaches this overlap. This is the only fixture in
// the file that does.
func TestAdvance_OrderGatedPrecedesTheCrossing(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Assessing {
		t.Errorf("State = %v, want Assessing — a paused job must park, not cross the boundary", got)
	}
	if j.HoldsLease() {
		t.Error("a gated job at the crossing still holds its lease; park must release it")
	}
	if q.slots.holds(j.ID()) {
		t.Error("a gated job at the crossing still holds its slot; park must release it")
	}
}

// TestAdvance_DemotionReleasesTheSlot pins the one demotion in the work spine.
// Assessing → Fetching is legal (par2 decided more blocks are needed) and it
// moves the job from a state that needs a slot to one that does not: §3.4 gives
// Fetching no slot because it is network-bound.
//
// grantFor cannot catch this. It acquires what the DESTINATION requires and is
// silent about what the ORIGIN held, so the job kept a pool-B slot for the
// whole download — minutes to hours — with every test green.
func TestAdvance_DemotionReleasesTheSlot(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if !q.slots.holds(j.ID()) {
		t.Fatal("fixture holds no slot at Assessing; this test cannot observe the leak")
	}
	if err := j.SetNext(job.Fetching); err != nil {
		t.Fatalf("SetNext: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := j.Snapshot().State.State; got != job.Fetching {
		t.Fatalf("State = %v, want Fetching; the demotion did not happen and the "+
			"slot assertion below would pass for the wrong reason", got)
	}
	if q.slots.holds(j.ID()) {
		t.Error("still holds a compute slot in Fetching; §3.4 gives Fetching none")
	}
	if !j.HoldsLease() {
		t.Error("lost its lease on a demotion; Fetching needs one and the job had it")
	}
}

// TestAdvance_AlreadyRunningJobIsLeftAlone asserts an already-running,
// UNGATED job's slot count and position are unchanged after Advance. It does
// NOT discriminate branch 2's `if q.holds(j.ID(), s) { return nil }` early
// return: neutering it (`if false && q.holds(...)`) still reports this test
// `ok`, because for an ungated running job the fall-through path is
// gatedBy (not gated) → grantFor(j, s.State.State), and grantFor is
// idempotent for a job that already holds everything the state requires —
// slotPool.acquire and the lease check both no-op on what is already held
// (pool.go). The two versions produce an identical observable result for
// this fixture, so no assertion reachable from an ungated fixture can pin
// this early return.
//
// The early return only becomes observable under gating, where the
// alternative path is park rather than an idempotent re-grant:
// TestAdvance_OrderGatedButAlreadyRunningIsNotParked below is what actually
// discriminates it.
func TestAdvance_AlreadyRunningJobIsLeftAlone(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	before := q.slots.outstanding()
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := q.slots.outstanding(); got != before {
		t.Errorf("slots outstanding = %d, want unchanged %d — an already-running job must not be touched", got, before)
	}
	if got := j.Snapshot().State.State; got != job.Assessing {
		t.Errorf("State = %v, want Assessing — an already-running job must not move", got)
	}
}

// TestAdvance_OrderGatedButAlreadyRunningIsNotParked is the overlap case task
// 7's brief calls for: a fixture where BOTH the "already holds/running" check
// and the "gated" check would independently be satisfied. §3.6 puts the
// holds() check first in branch 2 specifically so a live worker is never
// stripped of its resources out from under it, even if the job has since been
// paused. Reordering these two checks — running() first vs. gatedBy() first —
// changes ONLY this case's outcome; every other test in this file is agnostic
// to that order, which is why this case exists as its own test rather than as
// an assertion tacked onto one of the others.
func TestAdvance_OrderGatedButAlreadyRunningIsNotParked(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing) // holds lease + slot, Next unset: running
	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !j.HoldsLease() {
		t.Error("a running, gated job was parked; holds() must be checked before gatedBy()")
	}
	if !q.slots.holds(j.ID()) {
		t.Error("a running, gated job's slot was released; holds() must be checked before gatedBy()")
	}
}

// TestAdvance_OrderCancelPrecedesEveryOtherBranch pins that the IntentCancel
// check runs before branch 1, ahead of even a never-run job. A never-run,
// cancelled job satisfies BOTH "s.State.State == StateUnset" (branch 1) and
// "s.Intent == IntentCancel" (the cancel check): if branch 1 ran first it
// would call BeginAttempt on a job the user has asked to cancel, resurrecting
// an attempt that finishCancel's own never-run guard says should stay
// unstarted.
func TestAdvance_OrderCancelPrecedesEveryOtherBranch(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if j.HasRun() {
		t.Error("Advance started a cancelled, never-run job; the cancel check must precede branch 1")
	}
}

// TestRetry_ReopensASettledAttemptWithoutALease pins Retry's contract: it
// takes no lease (D-I12), so it must succeed even when pool A is fully
// exhausted by another job.
func TestRetry_ReopensASettledAttemptWithoutALease(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{})
	other := job.New("other", "n", job.Policy{})
	if err := q.Advance(other); err != nil { // opens other's attempt
		t.Fatalf("Advance(other): %v", err)
	}
	if err := q.Advance(other); err != nil { // grants other the only lease
		t.Fatalf("Advance(other) grant: %v", err)
	}
	if !other.HoldsLease() {
		t.Fatal("fixture: other does not hold the lease; pool A is not actually exhausted")
	}

	j := job.New("j1", "n", job.Policy{})
	mustAdvanceToSettled(t, q, j, job.OutcomeFailed)
	before := j.Attempts()
	if err := q.Retry(j); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if j.Attempts() != before+1 {
		t.Errorf("Attempts = %d, want %d — Retry must open a new attempt without a lease", j.Attempts(), before+1)
	}
	if j.HoldsLease() {
		t.Error("Retry granted a lease; it must take none (D-I12)")
	}
}

// TestRetry_ReleasesTheSettledAttemptsSlot pins the final whole-branch
// review's Critical 2: Retry must release the settled attempt's compute slot
// before opening the new attempt at Fetching. Fetching's needsSlot is false
// (§3.4), so nothing on the Fetching path ever calls releaseFor again — a
// slot carried in by Retry is held for the entire re-download, the same cost
// the Assessing→Fetching demotion leak had before TestAdvance_DemotionReleasesTheSlot
// was written to pin it.
func TestRetry_ReleasesTheSettledAttemptsSlot(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if !q.slots.holds(j.ID()) {
		t.Fatal("fixture holds no slot at Assessing; this test cannot observe the leak")
	}
	l, err := j.Finish(job.OutcomeFailed, testClock())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := q.reclaim(l); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if err := q.Retry(j); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if q.slots.holds(j.ID()) {
		t.Error("Retry carried the settled attempt's slot into the new one")
	}
	if err := q.Advance(j); err != nil { // branch 2: grants for Fetching
		t.Fatalf("Advance: %v", err)
	}
	st := j.Snapshot().State.State
	if got := q.slots.holds(j.ID()); got && !needsSlot(st) {
		t.Errorf("holds a slot at %v, which needsSlot=false; outstanding=%d", st, q.slots.outstanding())
	}
}

// TestRetry_RefusesALiveUnsettledJob pins B1: Retry on a job whose current
// attempt is open and unsettled — a live worker at Assessing, Repairing,
// Extracting or Finalizing — must refuse rather than strip that worker's
// slot. The prior version released unconditionally and then called
// j.BeginAttempt, which itself no-ops on an open attempt (job.go), leaving
// the release as the only observable effect: a running job's slot vanished
// from q.slots.held while the worker kept using it.
func TestRetry_RefusesALiveUnsettledJob(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if !q.slots.holds(j.ID()) {
		t.Fatal("fixture holds no slot at Assessing; this test cannot observe the strip")
	}
	if err := q.Retry(j); !errors.Is(err, ErrNotSettled) {
		t.Errorf("Retry = %v, want ErrNotSettled", err)
	}
	if !q.slots.holds(j.ID()) {
		t.Error("Retry stripped a live, unsettled job's slot")
	}
	if got := j.Snapshot().State.State; got != job.Assessing {
		t.Errorf("State = %v, want Assessing — Retry must not touch a live job", got)
	}
}

// TestRetry_RefusesANeverRunJob pins the other half of B1's fix: Retry is the
// door for a SETTLED attempt (§5.9, §5.10), not for a job that has never run
// at all — Advance's branch 1 owns starting one of those.
func TestRetry_RefusesANeverRunJob(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Retry(j); !errors.Is(err, ErrNotSettled) {
		t.Errorf("Retry = %v, want ErrNotSettled", err)
	}
	if j.HasRun() {
		t.Error("Retry started a never-run job; that is branch 1's job, not Retry's")
	}
}

// TestGrantFor_AcquiresBothWhenBothAreAvailable calls grantFor directly
// rather than only observing it through Advance. It asserts grantFor
// acquires both the lease and the slot when both pools have capacity — it
// does NOT pin the lease-before-slot ORDER grantFor's own doc comment
// claims: at capacity (1, 1) both acquisitions succeed regardless of which
// runs first, so swapping the two lines inside grantFor still reports this
// test `ok`. Order is only observable under contention, where the two
// orderings diverge on which pool gets touched before the call fails.
// TestGrantFor_NoLeaseCapacityFailsWithoutTouchingTheSlot below is what
// pins the order: with zero lease capacity, the swapped (slot-first)
// version would acquire the slot before failing on the lease, so that
// test's "slot untouched" assertion goes red under the swap while this one
// does not.
func TestGrantFor_AcquiresBothWhenBothAreAvailable(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if !q.grantFor(j, job.Assessing) {
		t.Fatal("grantFor(Assessing) = false, want true — one lease and one slot were available")
	}
	if !j.HoldsLease() {
		t.Error("grantFor did not grant the lease")
	}
	if !q.slots.holds(j.ID()) {
		t.Error("grantFor did not acquire the slot")
	}
}

// TestGrantFor_NoLeaseCapacityFailsWithoutTouchingTheSlot pins the ordering
// claim from the other side: when pool A has no capacity, grantFor must
// return false without ever touching pool B.
func TestGrantFor_NoLeaseCapacityFailsWithoutTouchingTheSlot(t *testing.T) {
	q := New(0, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := j.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if q.grantFor(j, job.Assessing) {
		t.Fatal("grantFor(Assessing) = true, want false — pool A has no capacity")
	}
	if q.slots.holds(j.ID()) {
		t.Error("grantFor acquired a slot despite failing to get a lease")
	}
}

// TestGrantFor_ReturnsIssuedLeaseOnGrantFailure pins the fifth Grant refusal
// grantFor's own comment enumerates: a lease minted from the pool but refused
// by the job (here, because the job has no open attempt) must go back to the
// pool rather than leak. Constructed by calling grantFor directly, because
// Advance never calls it for a job with no open attempt.
func TestGrantFor_ReturnsIssuedLeaseOnGrantFailure(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{}) // never began: no open attempt
	if q.grantFor(j, job.Fetching) {
		t.Fatal("grantFor = true, want false — Grant must refuse a job with no open attempt")
	}
	if q.leases.outstanding() != 0 {
		t.Errorf("leases outstanding = %d, want 0 — the minted lease must be reclaimed, not leaked", q.leases.outstanding())
	}
}

// TestPark_ReleasesBothPools calls Park directly rather than only observing
// it through Advance's gated branches, pinning its two-line contract:
// releaseFor then reclaim.
func TestPark_ReleasesBothPools(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)
	if err := q.Park(j); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if j.HoldsLease() {
		t.Error("Park left the lease held")
	}
	if q.slots.holds(j.ID()) {
		t.Error("Park left the slot held")
	}
}

// TestPark_PropagatesAForeignLeaseReclaimError pins Park's documented
// divergence from the brief's original signature (see Park's doc comment):
// a lease this pool never issued must surface as an error, not be silently
// swallowed the way a `return nil`-shaped Park would.
func TestPark_PropagatesAForeignLeaseReclaimError(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{})
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
		t.Fatal("could not issue a lease from the foreign pool")
	}
	if err := j.Grant(l); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := q.Park(j); !errors.Is(err, ErrNotOutstanding) {
		t.Errorf("Park = %v, want ErrNotOutstanding — a foreign lease's reclaim failure must surface", err)
	}
}

// TestPark_TakesTheQueueLock is B4's race-detector pin. Park mutates both
// pools (releaseFor touches q.slots, reclaim touches q.leases via
// j.Surrender) and doc.go/spec §5.1-§5.2 designate it as what a real
// dispatcher calls directly on a worker's yield — outside of any lock Advance
// holds. Before the fix, Park took no lock of its own and relied entirely on
// its caller already holding q.mu, which a direct call does not do:
// concurrent calls to Park on the same Queue raced on q.slots.held and
// q.leases.issued.
//
// Both goroutines call Park (not Advance) on two DIFFERENT jobs that each
// hold a slot and a lease, so both concurrently write into the same
// q.slots.held and q.leases.issued maps — that write/write overlap is what
// the race detector actually needs to catch anything; an unlocked Park
// racing an Advance call whose first branch never touches either pool (a
// never-run job's BeginAttempt path) proved nothing, and an earlier draft of
// this test used exactly that shape and stayed green for 5 straight -race
// runs even with the lock removed. Run with `go test -race`; without Park
// taking q.mu, this reports a DATA RACE rather than merely passing, so a
// plain (non-race) run of this test does not discriminate the fix at all.
func TestPark_TakesTheQueueLock(t *testing.T) {
	q := New(2, 2, testClock, &stubWorkers{})
	a := job.New("a", "n", job.Policy{})
	b := job.New("b", "n", job.Policy{})
	mustAdvanceTo(t, q, a, job.Assessing)
	mustAdvanceTo(t, q, b, job.Assessing)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := q.Park(a); err != nil {
			t.Errorf("Park(a): %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := q.Park(b); err != nil {
			t.Errorf("Park(b): %v", err)
		}
	}()
	wg.Wait()
}

// TestParkLocked_AssumesTheCallerAlreadyHoldsTheLock pins parkLocked's own
// contract directly: unlike Park, it must NOT take q.mu itself — Advance
// calls it from inside Advance's own q.mu span (branch 2 and branch 3's
// gated arms), and a parkLocked that tried to lock again would deadlock
// there. Calling it here with q.mu already held, from the test goroutine
// itself, is exactly that shape; a version of parkLocked that (incorrectly)
// re-acquired q.mu would hang this test rather than merely fail it.
func TestParkLocked_AssumesTheCallerAlreadyHoldsTheLock(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)

	q.mu.Lock()
	err := q.parkLocked(j)
	q.mu.Unlock()

	if err != nil {
		t.Fatalf("parkLocked: %v", err)
	}
	if j.HoldsLease() {
		t.Error("parkLocked left the lease held")
	}
	if q.slots.holds(j.ID()) {
		t.Error("parkLocked left the slot held")
	}
}

// TestAdvance_BranchTwo_ParkingAGatedNotYetRunningJobReturnsItsLease pins
// branch 2's OWN parkLocked call (the one guarding "current state's work is
// unfinished"), which is a different call site from branch 3's and is not
// exercised by TestAdvance_ParkingAGatedJobReturnsItsLease above — that test
// calls SetNext before pausing, which sets s.State.Next and routes it into
// branch 3 instead. A red check on branch 2's `return q.parkLocked(j)`
// replaced with `return nil` left every other test in this file green,
// confirming the gap: this test is what closes it.
//
// The fixture holds the lease but not the slot — running() (via holds())
// must be false for gatedBy to be consulted at all (§3.6's holds-before-gated
// order, pinned separately by TestAdvance_OrderGatedButAlreadyRunningIsNotParked),
// so the job cannot be holding everything Assessing needs. Releasing the slot
// only, directly on q.slots, simulates a job that holds a lease across a
// restart but has lost its in-memory compute-slot record — the same
// technique TestCancel_PostBoundaryNotRunningSettlesAtOnce documents, applied
// to one pool instead of both so the OTHER resource (the lease) stays held
// and observable.
func TestAdvance_BranchTwo_ParkingAGatedNotYetRunningJobReturnsItsLease(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing) // holds lease + slot
	q.slots.release(j.ID())               // simulate the slot record lost; lease stays held
	if q.holds(j.ID(), j.Snapshot()) {
		t.Fatal("fixture still holds everything Assessing needs; this test cannot reach branch 2's gated arm")
	}
	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if got := j.Snapshot().State.Next; got != job.StateUnset {
		t.Fatalf("fixture: Next = %v, want StateUnset — this must route through branch 2, not branch 3", got)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if j.HoldsLease() {
		t.Error("branch 2 left a gated, not-yet-running job holding its lease")
	}
	if q.leases.outstanding() != 0 {
		t.Errorf("leases outstanding = %d after branch 2 parks a gated job, want 0", q.leases.outstanding())
	}
}

// TestPark_IsTotalOverEveryShape pins that the exported door is
// unconditional and safe on every job a caller can hand it. An earlier draft
// of the RFC proposed refusing a non-gated job with an errNotGated sentinel,
// by analogy to Retry's ErrNotSettled. That analogy is wrong: gatedBy reads
// Intent and q.paused and consults NOTHING about worker liveness, so a gated
// job whose worker is still mid-article passes such a check and is stripped
// anyway, while legitimate non-gate returns — teardown, shutdown, a dead
// connection — are refused. It would protect against nothing and forbid
// something real.
//
// Totality is structural, not accidental: slotPool.release is a map delete,
// Surrender returns nil when nothing is held, and reclaim no-ops on nil (the
// last introduced by §3.9 for the paused-then-crossing case).
func TestPark_IsTotalOverEveryShape(t *testing.T) {
	t.Run("never run", func(t *testing.T) {
		q := New(2, 2, testClock, &stubWorkers{})
		j := job.New("a", "n", job.Policy{})
		if err := q.Park(j); err != nil {
			t.Fatalf("Park on a never-run job: %v", err)
		}
	})

	t.Run("already parked", func(t *testing.T) {
		q := New(2, 2, testClock, &stubWorkers{})
		j := job.New("b", "n", job.Policy{})
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (begin): %v", err)
		}
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (grant): %v", err)
		}
		if err := q.Park(j); err != nil {
			t.Fatalf("first Park: %v", err)
		}
		if err := q.Park(j); err != nil {
			t.Fatalf("second Park: %v — the door must be idempotent", err)
		}
		if got := q.leases.outstanding(); got != 0 {
			t.Errorf("leases outstanding = %d, want 0", got)
		}
	})

	t.Run("settled", func(t *testing.T) {
		q := New(2, 2, testClock, &stubWorkers{})
		j := job.New("c", "n", job.Policy{})
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (begin): %v", err)
		}
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (grant): %v", err)
		}
		if err := q.Settle(j, job.OutcomeFailed); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if err := q.Park(j); err != nil {
			t.Fatalf("Park on a settled job: %v", err)
		}
	})

	t.Run("work ended, next set", func(t *testing.T) {
		q := New(2, 2, testClock, &stubWorkers{})
		j := job.New("d", "n", job.Policy{})
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (begin): %v", err)
		}
		if err := q.Advance(j); err != nil {
			t.Fatalf("Advance (grant): %v", err)
		}
		if err := j.SetNext(job.Assessing); err != nil {
			t.Fatalf("SetNext: %v", err)
		}
		if err := q.Park(j); err != nil {
			t.Fatalf("Park with next set: %v", err)
		}
		if got := j.Snapshot().State.Next; got != job.Assessing {
			t.Errorf("Next = %v, want Assessing — Park releases resources, it does "+
				"not discard the verdict the finished work recorded", got)
		}
		if got := q.leases.outstanding(); got != 0 {
			t.Errorf("leases outstanding = %d, want 0", got)
		}
	})
}
