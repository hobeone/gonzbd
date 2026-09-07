package sched

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestPause_TakesTheQueueLock is the final review's race-detector pin for
// Pause and Paused, in TestPark_TakesTheQueueLock's own shape
// (advance_test.go): one goroutine writes q.paused through Pause while
// another reads it back through Paused, with no synchronizing call between
// the write and the read. Run with `go test -race`; without the lock on
// both doors this reports a DATA RACE, so a plain (non-race) run does not
// discriminate the fix at all.
//
// This was previously combined with Resume in one goroutine
// (q.Pause(); q.Resume()) racing against a single q.Paused() call in another.
// Resume's own q.mu.Lock/Unlock synchronizes-with Paused's lock acquisition
// — establishing write(Pause) → unlock(Resume) → lock(Paused) → read — so
// ThreadSanitizer's vector clock advances past the unlocked Pause write and
// it stops being a reportable race at all. Measured empirically: five single
// `-race -count=1` runs against a build with Pause's lock removed gave FAIL,
// FAIL, ok, FAIL, ok (~40% pass rate against genuinely broken code), and a
// `-count=50` run caught it only once across all 50 iterations. Splitting
// Pause and Resume into their own tests, each racing only against Paused
// with nothing in between, removes that synchronizing edge.
func TestPause_TakesTheQueueLock(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		q.Pause()
	}()
	go func() {
		defer wg.Done()
		q.Paused()
	}()
	wg.Wait()
}

// TestResume_TakesTheQueueLock is Resume's half of the same pin — see
// TestPause_TakesTheQueueLock's comment for why the two must be separate
// tests rather than one goroutine calling both doors. q.Pause() runs to
// completion before the two racing goroutines start, so it does not
// synchronize with either of them; it only sets up the flag Resume then
// clears.
func TestResume_TakesTheQueueLock(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	q.Pause()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		q.Resume()
	}()
	go func() {
		defer wg.Done()
		q.Paused()
	}()
	wg.Wait()
}

type stubWorkers struct{ aborted []string }

func (s *stubWorkers) Abort(j *job.Job) {
	if j != nil {
		s.aborted = append(s.aborted, j.ID())
	}
}

func testClock() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// walkPredicatesFor drives q.gatedBy and q.waitReason(id, ...) across the
// full Snapshot product space, for TestPredicatesArePure's two
// configurations.
func walkPredicatesFor(q *Queue, id string) {
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
							q.waitReason(id, s)
						}
					}
				}
			}
		}
	}
}

// TestPredicatesArePure is spec §6 test 1. It calls both predicates across the
// product space and asserts occupancy is unchanged. Acquisition leaking into
// the render path is the failure mode that does not show up as a wrong
// answer — it shows up as capacity disappearing while someone looks at a
// page.
//
// It runs the walk under two pool configurations, not one. At capacity
// (leaseCap=1, slotCap=1, one unit already occupied), leasePool.issue and
// slotPool.acquire both return early WITHOUT mutating anything — so a
// predicate that started calling q.slots.acquire(id) internally would return
// false, no map would change, outstanding() would hold steady, and this
// test would stay green against the exact regression its own doc comment
// names. "headroom" gives each pool a second unit so a stray acquire
// actually mutates: outstanding() moves, and — since acquire is
// idempotent — q.slots.holds(id) latches true and stays true regardless of
// how many further calls the walk makes for the same id, catching a leak
// that a lone before/after outstanding() comparison could otherwise miss if
// some other call in the walk released it back down in between.
func TestPredicatesArePure(t *testing.T) {
	for _, tc := range []struct {
		name              string
		leaseCap, slotCap int
	}{
		{"exhausted", 1, 1},
		{"headroom", 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := New(tc.leaseCap, tc.slotCap, testClock, &stubWorkers{})
			if q.leases.issue() == nil || !q.slots.acquire("other") {
				t.Fatal("could not occupy one unit of each pool for the test")
			}
			beforeL, beforeS := q.leases.outstanding(), q.slots.outstanding()

			walkPredicatesFor(q, "j1")

			if q.leases.outstanding() != beforeL || q.slots.outstanding() != beforeS {
				t.Errorf("pools moved during a render-path walk: leases %d→%d, slots %d→%d",
					beforeL, q.leases.outstanding(), beforeS, q.slots.outstanding())
			}
			if q.slots.holds("j1") {
				t.Error(`q.slots.holds("j1") = true after a render-path walk; a predicate acquired ` +
					"a slot for the job under test")
			}
		})
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
	if err := q.reclaim(l); !errors.Is(err, ErrNotOutstanding) {
		t.Errorf("reclaim(foreign lease) = %v, want ErrNotOutstanding", err)
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
// target state does not need one, including at job.StateUnset (used by Park,
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

// TestNew_PanicsOnNilWorkers pins New's precondition: w must not be nil.
// cancel.go's interrupt arm dereferences q.work unconditionally, so a nil
// Workers is a construction-time programmer error New refuses immediately
// rather than let it surface as a nil-pointer dereference deep inside a
// later Cancel call.
func TestNew_PanicsOnNilWorkers(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New(nil Workers) did not panic")
		}
	}()
	New(1, 1, testClock, nil)
}

// TestNew_PanicsOnNilClock is the clock half of the same precondition. Without
// it New succeeds and the panic surfaces later, inside q.now(), on whichever of
// Advance/Cancel/Retry happens to run first — a stack trace that points at the
// scheduler rather than at the caller that built it wrong.
func TestNew_PanicsOnNilClock(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New(nil clock) did not panic")
		}
	}()
	New(1, 1, nil, &stubWorkers{})
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

// TestPause_MakesGlobalPauseReachable pins the gap this closes. gatedBy reads
// q.paused, and before these doors nothing in the package could write it — so
// job.GlobalPause existed as a WaitReason that no production code path could
// ever produce.
func TestPause_MakesGlobalPauseReachable(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (begin): %v", err)
	}
	s := j.Snapshot()
	if _, gated := q.gatedBy(s); gated {
		t.Fatal("a fresh Queue must not be gated")
	}

	q.Pause()
	if !q.Paused() {
		t.Error("Paused() = false after Pause()")
	}
	r, gated := q.gatedBy(j.Snapshot())
	if !gated || r != job.GlobalPause {
		t.Errorf("gatedBy = (%v, %v), want (GlobalPause, true)", r, gated)
	}

	q.Resume()
	if q.Paused() {
		t.Error("Paused() = true after Resume()")
	}
	if _, gated := q.gatedBy(j.Snapshot()); gated {
		t.Error("gatedBy still gated after Resume()")
	}
}

// TestPause_DoesNotTakeResourcesFromAHoldingJob pins the contract Decision 3
// puts on B2's dispatcher. §8.3: gating never interrupts work. Pause sets a
// flag; it cannot sweep, because the Queue holds no jobs to sweep. A holding
// job keeps everything until its worker returns and the dispatcher Parks it.
func TestPause_DoesNotTakeResourcesFromAHoldingJob(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (grant): %v", err)
	}

	q.Pause()
	if err := q.Advance(j); err != nil { // a tick under global pause
		t.Fatalf("Advance under pause: %v", err)
	}
	if !j.Snapshot().HoldsLease {
		t.Error("Pause took the lease from a holding job — §8.3: gating never " +
			"interrupts work; only Park releases it")
	}

	if err := q.Park(j); err != nil { // the dispatcher's half of the contract
		t.Fatalf("Park: %v", err)
	}
	if got := q.leases.outstanding(); got != 0 {
		t.Errorf("leases outstanding after Park = %d, want 0", got)
	}
}

func TestQueue_SetCaps_LeaseCap_SlotCap(t *testing.T) {
	q := New(2, 3, testClock, &stubWorkers{})
	if got := q.LeaseCap(); got != 2 {
		t.Errorf("LeaseCap() = %d, want 2", got)
	}
	if got := q.SlotCap(); got != 3 {
		t.Errorf("SlotCap() = %d, want 3", got)
	}

	q.SetCaps(5, 7)
	if got := q.LeaseCap(); got != 5 {
		t.Errorf("after SetCaps(5, 7), LeaseCap() = %d, want 5", got)
	}
	if got := q.SlotCap(); got != 7 {
		t.Errorf("after SetCaps(5, 7), SlotCap() = %d, want 7", got)
	}

	// Clamp to 1
	q.SetCaps(0, -1)
	if got := q.LeaseCap(); got != 1 {
		t.Errorf("after SetCaps(0, -1), LeaseCap() = %d, want 1", got)
	}
	if got := q.SlotCap(); got != 1 {
		t.Errorf("after SetCaps(0, -1), SlotCap() = %d, want 1", got)
	}
}
