package sched

import (
	"errors"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

type stubWorkers struct{ aborted []string }

func (s *stubWorkers) Abort(jobID string) { s.aborted = append(s.aborted, jobID) }

func testClock() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// TestPredicatesArePure is spec §6 test 1. It calls both predicates across the
// product space with BOTH pools exhausted and asserts occupancy is unchanged.
// Acquisition leaking into the render path is the failure mode that does not
// show up as a wrong answer — it shows up as capacity disappearing while
// someone looks at a page.
func TestPredicatesArePure(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	if q.leases.issue() == nil || !q.slots.acquire("other") {
		t.Fatal("could not exhaust the pools for the test")
	}
	beforeL, beforeS := q.leases.outstanding(), q.slots.outstanding()
	for _, st := range append(job.AllStates(), job.StateUnset) {
		for _, nx := range append(job.AllStates(), job.StateUnset) {
			for _, o := range job.AllOutcomes() {
				for _, in := range job.AllIntents() {
					for _, holdsLease := range []bool{false, true} {
						for _, paused := range []bool{false, true} {
							q.paused = paused
							s := job.Snapshot{
								State:      job.StateView{State: st, Next: nx, Outcome: o},
								Intent:     in,
								HoldsLease: holdsLease,
								HasRun:     st != job.StateUnset,
							}
							q.gatedBy(s)
							q.waitReason("j1", s)
						}
					}
				}
			}
		}
	}
	if q.leases.outstanding() != beforeL || q.slots.outstanding() != beforeS {
		t.Errorf("pools moved during a render-path walk: leases %d→%d, slots %d→%d",
			beforeL, q.leases.outstanding(), beforeS, q.slots.outstanding())
	}
}

// TestGatePrecedenceIsTotal is spec §6 test 2. It walks
// Intent × globalPause × holdsLease × holdsSlot × State and asserts waitReason
// agrees with §3.4 at every point, rather than at the handful of points a
// hand-written case list would reach.
//
// The expected value is recomputed by expectedWait, which shares no code with
// waitReason, gatedBy, running or holds — the four functions under test. It
// DOES call needsLease and needsSlot, and that is a deliberate, bounded
// exception: those two are pinned against a literal table in
// TestRequirements_AreTotal, so they are not deciding this test's answer by
// the same reasoning the code uses. Restating them a third time here would put
// §3.4's resource rules in three places, which is the failure this plan exists
// to stop. Stated rather than left implicit, because "the oracle is
// independent" is exactly the kind of claim that has been wrong here before.
func TestGatePrecedenceIsTotal(t *testing.T) {
	states := append(job.AllStates(), job.StateUnset)
	for _, paused := range []bool{false, true} {
		for _, in := range job.AllIntents() {
			if in == job.IntentCancel {
				// Absent from gatedBy on purpose: advance handles cancel first,
				// so no cancel value reaches the render path (§3.4).
				continue
			}
			for _, st := range states {
				for _, holdsLease := range []bool{false, true} {
					for _, holdsSlot := range []bool{false, true} {
						for _, settled := range []bool{false, true} {
							q := New(1, 1, testClock, &stubWorkers{})
							q.paused = paused
							if holdsSlot {
								q.slots.acquire("j1")
							}
							o := job.OutcomePending
							if settled {
								o = job.OutcomeFailed
							}
							s := job.Snapshot{
								State:      job.StateView{State: st, Outcome: o},
								Intent:     in,
								HoldsLease: holdsLease,
								HasRun:     st != job.StateUnset,
							}
							gotR, gotW := q.waitReason("j1", s)
							wantR, wantW := expectedWait(st, o, in, paused, holdsLease, holdsSlot)
							if gotR != wantR || gotW != wantW {
								t.Errorf("waitReason(state=%v settled=%v intent=%v paused=%v lease=%v slot=%v) = %v,%v; want %v,%v",
									st, settled, in, paused, holdsLease, holdsSlot, gotR, gotW, wantR, wantW)
							}
						}
					}
				}
			}
		}
	}
}

// expectedWait is §3.4 written out longhand, independently of waitReason.
func expectedWait(st job.State, o job.Outcome, in job.Intent, paused, lease, slot bool) (job.WaitReason, bool) {
	if o.IsSettled() {
		return 0, false
	}
	open := st != job.StateUnset
	holdsAll := (!needsLease(st) || lease) && (!needsSlot(st) || slot)
	if open && holdsAll { // next is always unset in this walk
		return 0, false
	}
	if in == job.IntentPause {
		return job.UserPaused, true
	}
	if paused {
		return job.GlobalPause, true
	}
	if st == job.StateUnset {
		return job.NoLease, true
	}
	if needsLease(st) && !lease {
		return job.NoLease, true
	}
	return job.NoComputeSlot, true
}

// TestWaitReason_SettledAndNeverRunReturnEarly pins the two early returns §3.4
// calls "not decoration". Without them a settled job reports NoComputeSlot —
// waiting for a slot it will never use — and a never-run job reports
// NoComputeSlot when it is waiting for a lease.
func TestWaitReason_SettledAndNeverRunReturnEarly(t *testing.T) {
	q := New(0, 0, testClock, &stubWorkers{})
	settled := job.Snapshot{
		State:  job.StateView{State: job.Fetching, Outcome: job.OutcomeFailed},
		Intent: job.IntentRun, HasRun: true,
	}
	if r, waiting := q.waitReason("j1", settled); waiting {
		t.Errorf("waitReason(settled at Fetching) = %v, true; a settled attempt waits for nothing "+
			"even though its position still requires a lease (§3.4, corrected by change 03)", r)
	}
	never := job.Snapshot{Intent: job.IntentRun}
	r, waiting := q.waitReason("j2", never)
	if !waiting || r != job.NoLease {
		t.Errorf("waitReason(never run) = %v, %v; want NoLease, true", r, waiting)
	}
}

// TestQueue_Running pins §3.4's three-conjunct definition directly: open AND
// holds AND next-unset. Each case flips exactly one conjunct false, so a
// mutation collapsing any one of the three ANDs into an OR (or dropping a
// conjunct) shows up as a case going the wrong way.
func TestQueue_Running(t *testing.T) {
	openHoldingNoNext := job.Snapshot{
		State:      job.StateView{State: job.Fetching, Outcome: job.OutcomePending},
		HoldsLease: true, HasRun: true,
	}
	q := New(1, 0, testClock, &stubWorkers{})
	if !q.running("j1", openHoldingNoNext) {
		t.Error("running(open, holds lease, next unset) = false, want true")
	}

	notOpen := job.Snapshot{
		State:  job.StateView{State: job.Fetching, Outcome: job.OutcomePending},
		HasRun: false, // never begun: not open
	}
	if q.running("j1", notOpen) {
		t.Error("running(not open) = true, want false")
	}

	missingResource := job.Snapshot{
		State:      job.StateView{State: job.Fetching, Outcome: job.OutcomePending},
		HoldsLease: false, HasRun: true, // Fetching needs a lease and lacks one
	}
	if q.running("j1", missingResource) {
		t.Error("running(open, does not hold what Fetching needs) = true, want false")
	}

	q2 := New(1, 1, testClock, &stubWorkers{})
	if !q2.slots.acquire("j1") {
		t.Fatal("could not acquire a slot for the test")
	}
	workEnded := job.Snapshot{
		State:      job.StateView{State: job.Assessing, Next: job.Fetching, Outcome: job.OutcomePending},
		HoldsLease: true, HasRun: true, // holds everything Assessing needs, but Next is set
	}
	if q2.running("j1", workEnded) {
		t.Error("running(next set) = true, want false")
	}
}

// TestWaitReason_UsesTheNextStateWhenWorkHasEnded pins §3.4's "which state's
// requirements to test" rule. Assessing{next: Fetching} after a NeedsMore
// verdict holds its lease and needs only that lease — testing Assessing's
// requirements reports NoComputeSlot for a job that should be granted at once.
func TestWaitReason_UsesTheNextStateWhenWorkHasEnded(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{})
	s := job.Snapshot{
		State:      job.StateView{State: job.Assessing, Next: job.Fetching, Outcome: job.OutcomePending},
		Intent:     job.IntentRun,
		HoldsLease: true,
		HasRun:     true,
	}
	if r, waiting := q.waitReason("j1", s); waiting {
		t.Errorf("waitReason(Assessing{next: Fetching}, holding a lease) = %v, true; "+
			"it waits on what FETCHING needs, and it already holds that", r)
	}
}

// TestWaitReason_PostBoundaryWaitsForASlotNotALease pins the bug revision 3
// shipped: an unconditional !HoldsLease() reports NoLease for a job that gave
// its lease up at the crossing and is in fact waiting for a compute slot.
func TestWaitReason_PostBoundaryWaitsForASlotNotALease(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{})
	s := job.Snapshot{
		State:  job.StateView{State: job.Extracting, Outcome: job.OutcomePending},
		Intent: job.IntentRun, HasRun: true,
	}
	r, waiting := q.waitReason("j1", s)
	if !waiting || r != job.NoComputeSlot {
		t.Errorf("waitReason(Extracting, no lease, no slot) = %v, %v; want NoComputeSlot, true — "+
			"Extracting does not need a lease, it surrendered one at the crossing", r, waiting)
	}
}

// TestReclaim_ReturnsErrNotOutstandingForAForeignLease pins reclaim's audit:
// a lease this queue's pool never issued must be rejected, and one it did
// issue must succeed and be gone from occupancy afterward.
func TestReclaim_ReturnsErrNotOutstandingForAForeignLease(t *testing.T) {
	q := New(1, 0, testClock, &stubWorkers{})
	foreign := New(1, 0, testClock, &stubWorkers{})
	l := foreign.leases.issue()
	if l == nil {
		t.Fatal("could not issue a lease from the foreign queue's pool")
	}
	if err := q.reclaim(l); !errors.Is(err, errNotOutstanding) {
		t.Errorf("reclaim(foreign lease) = %v, want errNotOutstanding", err)
	}

	mine := q.leases.issue()
	if mine == nil {
		t.Fatal("could not issue a lease from q's own pool")
	}
	if err := q.reclaim(mine); err != nil {
		t.Errorf("reclaim(own lease) = %v, want nil", err)
	}
	if got := q.leases.outstanding(); got != 0 {
		t.Errorf("leases.outstanding() = %d after reclaim, want 0", got)
	}
}

// TestReleaseFor pins releaseFor's rule: free the slot exactly when the
// target state does not need one, including at job.StateUnset (used by park,
// settlement and cancel to free everything without naming pool B), and leave
// it held when the target state still needs a slot.
func TestReleaseFor(t *testing.T) {
	q := New(0, 1, testClock, &stubWorkers{})
	j := job.New("j1", "job.nzb", job.Policy{})

	if !q.slots.acquire(j.ID()) {
		t.Fatal("could not acquire a slot for the test")
	}

	// Extracting still needs a slot: release must be a no-op.
	q.releaseFor(j, job.Extracting)
	if !q.slots.holds(j.ID()) {
		t.Fatal("releaseFor(Extracting) released a slot Extracting still needs")
	}

	// StateUnset needs nothing: release must free it.
	q.releaseFor(j, job.StateUnset)
	if q.slots.holds(j.ID()) {
		t.Error("releaseFor(StateUnset) left the slot held; StateUnset needs nothing")
	}
}

// TestQueue_NowReturnsTheInjectedClock pins that now() reads the clock the
// Queue was constructed with rather than wall time.
func TestQueue_NowReturnsTheInjectedClock(t *testing.T) {
	want := time.Date(2099, 5, 4, 3, 2, 1, 0, time.UTC)
	q := New(0, 0, func() time.Time { return want }, &stubWorkers{})
	if got := q.now(); !got.Equal(want) {
		t.Errorf("now() = %v, want %v (the injected clock, not wall time)", got, want)
	}
}

// TestQueue_MuSerializesAccess pins that Queue.mu is a working mutual-exclusion
// lock, ready for the Task 6/7 callers (Advance, Cancel, grantFor) that will
// hold it across a predicate and a pool mutation, even though no method in
// this file takes it yet — see the Queue struct's comment for why.
func TestQueue_MuSerializesAccess(t *testing.T) {
	q := New(0, 0, testClock, &stubWorkers{})
	q.mu.Lock()
	acquired := make(chan struct{})
	go func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("a second Lock succeeded while the first was still held")
	case <-time.After(20 * time.Millisecond):
	}

	q.mu.Unlock()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("the second Lock never succeeded after the first was released")
	}
}
