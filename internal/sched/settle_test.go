package sched

import (
	"errors"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// newSettleQueue builds a Queue with room for one of each resource and a job
// already open at Fetching holding a lease — the shape a worker settles from.
func newSettleQueue(t *testing.T) (*Queue, *job.Job) {
	t.Helper()
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil { // branch 1: BeginAttempt
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil { // branch 2: grant the Fetching lease
		t.Fatalf("Advance (grant): %v", err)
	}
	if !j.Snapshot().HoldsLease {
		t.Fatalf("setup: want a lease at Fetching, snapshot=%+v", j.Snapshot())
	}
	return q, j
}

// TestSettle_ReturnsBothPools pins the door's whole reason for existing: three
// exported job doors yield a *job.Lease and before this one, nothing exported
// could take it back.
func TestSettle_ReturnsBothPools(t *testing.T) {
	q, j := newSettleQueue(t)
	if err := q.Settle(j, job.OutcomeFailed); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := q.leases.outstanding(); got != 0 {
		t.Errorf("leases outstanding = %d, want 0 — a settled job needs none", got)
	}
	if got := q.slots.outstanding(); got != 0 {
		t.Errorf("slots outstanding = %d, want 0", got)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeFailed {
		t.Errorf("Outcome = %v, want Failed", got)
	}
}

// TestSettle_RefusesCancelledOutcome pins that only the latch authorises
// Cancelled. A caller minting it directly would settle a job that renders as
// Deleted while still carrying IntentRun, so q.Retry would reopen it — the
// resurrection D-I14's note says must have no path.
func TestSettle_RefusesCancelledOutcome(t *testing.T) {
	q, j := newSettleQueue(t)
	if err := q.Settle(j, job.OutcomeCancelled); !errors.Is(err, errCancelReserved) {
		t.Fatalf("Settle(Cancelled) = %v, want errCancelReserved", err)
	}
	if j.Snapshot().State.Outcome.IsSettled() {
		t.Error("refused Settle must not have settled the attempt")
	}
	if got := q.leases.outstanding(); got != 1 {
		t.Errorf("leases outstanding = %d, want 1 — a refused Settle releases nothing", got)
	}
}

// TestSettle_TakesTheQueueLock is the final review's race-detector pin for
// Settle, in TestPark_TakesTheQueueLock's own shape (advance_test.go):
// settleLocked mutates both pools (releaseFor touches q.slots, reclaim
// touches q.leases), and an earlier draft that called Settle on the SAME job
// from both goroutines, or paired it with a branch of Advance that never
// touches either pool, stayed green under -race even with the lock removed
// — a write/write overlap needs two DIFFERENT jobs settling concurrently.
// Run with `go test -race`; without Settle taking q.mu this reports a DATA
// RACE, so a plain (non-race) run does not discriminate the fix at all.
func TestSettle_TakesTheQueueLock(t *testing.T) {
	q := New(2, 2, testClock, &stubWorkers{})
	a := job.New("a", "n", job.Policy{})
	b := job.New("b", "n", job.Policy{})
	mustAdvanceTo(t, q, a, job.Assessing)
	mustAdvanceTo(t, q, b, job.Assessing)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := q.Settle(a, job.OutcomeFailed); err != nil {
			t.Errorf("Settle(a): %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := q.Settle(b, job.OutcomeFailed); err != nil {
			t.Errorf("Settle(b): %v", err)
		}
	}()
	wg.Wait()
}

// TestSettle_InterruptedCancelOverridesTheOutcome pins the case the door was
// designed around: Cancel on a running pre-boundary job calls Abort and
// returns without settling, so the worker comes back with an I/O error and the
// dispatcher reports Failed. Recording Failed would be false — we caused it.
func TestSettle_InterruptedCancelOverridesTheOutcome(t *testing.T) {
	q, j := newSettleQueue(t)
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Settle(j, job.OutcomeFailed); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeCancelled {
		t.Errorf("Outcome = %v, want Cancelled — Fetching is pre-boundary, so the "+
			"cancel interrupted this worker and its error is our artifact", got)
	}
}

// TestSettle_GatedCancelPreservesTheOutcome pins D-I11 from the other side. A
// running Finalizing job that is cancelled is GATED, not interrupted: it moves
// the files and runs the user script, then settles OK. Overriding to Cancelled
// would make the record contradict the disk.
func TestSettle_GatedCancelPreservesTheOutcome(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	driveToFinalizing(t, q, j)
	if err := j.SetIntent(job.IntentCancel); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	if err := q.Settle(j, job.OutcomeOK); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeOK {
		t.Errorf("Outcome = %v, want OK — D-I11: the files moved and the script ran, "+
			"so recording Cancelled here would be false", got)
	}
	if got := j.Intent(); got != job.IntentCancel {
		t.Errorf("Intent = %v, want IntentCancel — it survives to tell a consumer "+
			"the request came too late", got)
	}
}

// TestSettle_RefusedFinishErrors asserts exactly what its name says: an error
// comes back. It does NOT assert anything about release, because a never-run
// job holds neither pool — TestSettle_RefusedFinishRetainsTheLeaseAndSlot
// below is what pins the retention half of step 2, on a fixture that can
// actually observe a wrongful release.
//
// This test used to be named TestSettle_RefusedFinishReleasesNothing while
// asserting only this — a name-claim mismatch the final review found:
// inserting a release above settleLocked's `if err != nil` check LIVED
// against the old name's assertions, because a job holding nothing cannot
// distinguish "released" from "never acquired".
func TestSettle_RefusedFinishErrors(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("never-run", "n", job.Policy{})
	if err := q.Settle(j, job.OutcomeFailed); err == nil {
		t.Fatal("Settle on a job that never ran = nil, want an error from Finish")
	}
}

// TestSettle_RefusedFinishRetainsTheLeaseAndSlot pins the reachable case
// TestSettle_RefusedFinishErrors's never-run fixture cannot reach: a job
// that holds both a lease and a slot when Finish refuses it.
// admissibleAt[OutcomeOK] is {Finalizing} (internal/job/admissibility.go),
// so Settle(j, OutcomeOK) on a job open at Assessing — holding a lease and a
// slot, both of which Assessing requires — is refused by Finish while the
// job still occupies that position. A job stripped at a position that
// requires a lease is resourceless while still occupying it, which is
// exactly what step 2's ordering (release AFTER the error check, never
// before) exists to prevent.
func TestSettle_RefusedFinishRetainsTheLeaseAndSlot(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustHoldAt(t, q, j, job.Assessing)

	if err := q.Settle(j, job.OutcomeOK); err == nil {
		t.Fatal("Settle(OutcomeOK) on a job at Assessing = nil, want an error " +
			"— OutcomeOK is admissible only at Finalizing")
	}
	if got := q.leases.outstanding(); got != 1 {
		t.Errorf("leases outstanding = %d, want 1 — a refused Finish must not release "+
			"the lease Assessing still requires", got)
	}
	if !q.slots.holds(j.ID()) {
		t.Error("refused Finish released the slot — Assessing still requires it")
	}
}

// TestCancelInterrupts pins §8.4's interrupt/gate boundary directly, rather
// than only through cancelInterrupts's two callers (finishCancel and
// settleLocked). The expectation for each state is written out explicitly —
// not derived from job.IsProduction — so this does not degenerate into a
// restatement of cancelInterrupts's own one-line body.
func TestCancelInterrupts(t *testing.T) {
	tests := []struct {
		state job.State
		want  bool // true: cancel interrupts (pre-boundary); false: cancel gates (post-boundary)
	}{
		{job.StateUnset, true},
		{job.Fetching, true},
		{job.Assessing, true},
		{job.Repairing, true},
		{job.Extracting, false},
		{job.Finalizing, false},
	}
	for _, tc := range tests {
		t.Run(tc.state.String(), func(t *testing.T) {
			if got := cancelInterrupts(tc.state); got != tc.want {
				t.Errorf("cancelInterrupts(%v) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// driveToAssessing walks a job to a RUNNING Assessing: Fetching (holds a
// lease) → Assessing (holds that same lease AND a compute slot, per
// requirements.go's needsLease/needsSlot). It is the shape
// TestSettleLocked_ReleasesSlotEvenWhenReclaimFails needs: a job holding both
// resources settleLocked's four-step order returns.
func driveToAssessing(t *testing.T, q *Queue, j *job.Job) {
	t.Helper()
	if err := q.Advance(j); err != nil { // BeginAttempt
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil { // grant the Fetching lease
		t.Fatalf("Advance (grant): %v", err)
	}
	if err := j.SetNext(job.Assessing); err != nil {
		t.Fatalf("SetNext(Assessing): %v", err)
	}
	if err := q.Advance(j); err != nil { // move to Assessing, take a slot
		t.Fatalf("Advance (to Assessing): %v", err)
	}
	s := j.Snapshot()
	if s.State.State != job.Assessing || !s.HoldsLease || !q.slots.holds(j.ID()) {
		t.Fatalf("drive ended at %+v, holds lease=%v slot=%v, want Assessing holding both",
			s, s.HoldsLease, q.slots.holds(j.ID()))
	}
}

// TestSettleLocked_ReleasesSlotEvenWhenReclaimFails is a direct call to
// settleLocked — check_test_alignment requires one, and it is worth having on
// its own merits: it pins step 3 running unconditionally before step 4, per
// settleLocked's own doc comment ("release the compute slot BEFORE the
// reclaim ... turning one audit error into a permanent pool-B leak").
//
// The lease this job holds is pre-removed from the pool's issued set before
// settleLocked runs, so j.Finish still succeeds (the job itself is unaware)
// but the subsequent q.reclaim(l) fails its identity audit with
// errNotOutstanding. If step 3 ran after step 4 — or was skipped on a reclaim
// failure — the slot would still be held after this call returns.
func TestSettleLocked_ReleasesSlotEvenWhenReclaimFails(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	driveToAssessing(t, q, j)

	// The pool issued LeaseID 1 first (newLeasePool starts at zero, issue
	// increments before minting): `driveToAssessing` above is this job's
	// first and only Advance sequence, so its one grantFor call is the pool's
	// first issue. Reclaiming it here, out from under the job, simulates the
	// identity-audit failure without needing a second Queue.
	if err := q.leases.reclaim(job.NewLease(1)); err != nil {
		t.Fatalf("setup: pre-reclaim of lease 1: %v", err)
	}

	q.mu.Lock()
	err := q.settleLocked(j, job.OutcomeFailed, j.Snapshot())
	q.mu.Unlock()

	if !errors.Is(err, errNotOutstanding) {
		t.Fatalf("settleLocked error = %v, want errNotOutstanding", err)
	}
	if got := q.slots.outstanding(); got != 0 {
		t.Errorf("slots outstanding = %d, want 0 — step 3 must release the slot "+
			"even though step 4's reclaim then fails", got)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeFailed {
		t.Errorf("Outcome = %v, want Failed — Finish (step 2) still committed", got)
	}
}

// TestSettleLocked_RefusedFinishReleasesNothing is a direct call to
// settleLocked on a job with no open attempt: j.Finish returns an error
// before settleLocked's steps 3 and 4 run at all. Nothing was ever acquired
// for a job that never ran, so both pools stay empty — the direct-call
// counterpart of TestSettle_RefusedFinishErrors, which only reaches
// settleLocked through Settle.
func TestSettleLocked_RefusedFinishReleasesNothing(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("never-run", "n", job.Policy{})

	q.mu.Lock()
	err := q.settleLocked(j, job.OutcomeFailed, j.Snapshot())
	q.mu.Unlock()

	if err == nil {
		t.Fatal("settleLocked on a never-run job = nil, want an error from Finish")
	}
	if got := q.leases.outstanding(); got != 0 {
		t.Errorf("leases outstanding = %d, want 0", got)
	}
	if got := q.slots.outstanding(); got != 0 {
		t.Errorf("slots outstanding = %d, want 0", got)
	}
}

// driveToFinalizing walks a job along the work spine to a RUNNING Finalizing:
// Fetching → Assessing → cross to Extracting → Finalizing. It asserts at the
// end rather than trusting the walk, because a silent early exit would make
// every test built on it vacuous.
func driveToFinalizing(t *testing.T, q *Queue, j *job.Job) {
	t.Helper()
	if err := q.Advance(j); err != nil { // BeginAttempt
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil { // grant the Fetching lease
		t.Fatalf("Advance (grant): %v", err)
	}
	if err := j.SetNext(job.Assessing); err != nil {
		t.Fatalf("SetNext(Assessing): %v", err)
	}
	if err := q.Advance(j); err != nil { // move to Assessing, take a slot
		t.Fatalf("Advance (to Assessing): %v", err)
	}
	if err := j.SetNext(job.Extracting); err != nil {
		t.Fatalf("SetNext(Extracting): %v", err)
	}
	if err := q.Advance(j); err != nil { // Cross, reclaim the lease, grant a slot
		t.Fatalf("Advance (cross): %v", err)
	}
	if err := j.SetNext(job.Finalizing); err != nil {
		t.Fatalf("SetNext(Finalizing): %v", err)
	}
	if err := q.Advance(j); err != nil { // move to Finalizing
		t.Fatalf("Advance (to Finalizing): %v", err)
	}
	s := j.Snapshot()
	if s.State.State != job.Finalizing {
		t.Fatalf("drive ended at %v, want Finalizing", s.State.State)
	}
	if !q.running(j.ID(), s) {
		t.Fatalf("drive ended not running: snapshot=%+v slots=%d", s, q.slots.outstanding())
	}
}
