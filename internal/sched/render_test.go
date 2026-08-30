package sched

import (
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestRender_TakesTheQueueLock is the final review's race-detector pin for
// Render, in TestPark_TakesTheQueueLock's own shape (advance_test.go). It
// covers the read side that Settle's and Park's own pins do not: Render
// reads q.slots.holds(id) inside q.running while a concurrent Park on a
// DIFFERENT job deletes from that same map via releaseFor — a
// read/write overlap on q.slots.held, rather than the write/write overlap
// the other two doors' pins exercise. Run with `go test -race`; without
// Render taking q.mu this reports a DATA RACE, so a plain (non-race) run
// does not discriminate the fix at all.
func TestRender_TakesTheQueueLock(t *testing.T) {
	q := New(2, 2, testClock, &stubWorkers{})
	a := job.New("a", "n", job.Policy{})
	b := job.New("b", "n", job.Policy{})
	mustAdvanceTo(t, q, a, job.Assessing)
	mustAdvanceTo(t, q, b, job.Assessing)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		q.Render(a)
	}()
	go func() {
		defer wg.Done()
		if err := q.Park(b); err != nil {
			t.Errorf("Park(b): %v", err)
		}
	}()
	wg.Wait()
}

// TestRender_FillsEveryFieldFromOneSnapshot pins that Render is the seam
// job.RenderView's own doc comment describes: "Nothing in this package can
// answer that ... so this type is the seam ... Half B fills them for real."
func TestRender_FillsEveryFieldFromOneSnapshot(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (grant): %v", err)
	}

	v := q.Render(j)
	if v.State != job.Fetching {
		t.Errorf("State = %v, want Fetching", v.State)
	}
	if v.Next != job.StateUnset {
		t.Errorf("Next = %v, want StateUnset — the second Advance only granted "+
			"the Fetching lease, it did not end Fetching's work or set a next state",
			v.Next)
	}
	if v.Activity != job.ActNone {
		t.Errorf("Activity = %v, want ActNone — nothing in this fixture ever "+
			"calls SetActivity", v.Activity)
	}
	if v.Outcome != job.OutcomePending {
		t.Errorf("Outcome = %v, want OutcomePending — the attempt BeginAttempt "+
			"opened has not been settled by Finish", v.Outcome)
	}
	if v.Assessed {
		t.Error("Assessed = true, want false — this attempt has never reached " +
			"Assessing")
	}
	if !v.Running {
		t.Error("Running = false, want true — the job holds its Fetching lease " +
			"and its work has not ended")
	}
	if v.Reason != job.NoLease {
		t.Errorf("Reason = %v, want NoLease (0) — RenderView's doc comment "+
			"names NoLease's zero value as the reading a running job also "+
			"produces: waitReason returns (0, false) for a running job and "+
			"Render discards the boolean, so a running job's Reason reads "+
			"identically to \"waiting on pool A\" and must not be interpreted "+
			"without checking Running first", v.Reason)
	}
	if v.Intent != job.IntentRun {
		t.Errorf("Intent = %v, want IntentRun", v.Intent)
	}
}

// TestRender_DistinguishesRunningFromReadyToAdvance pins why one door replaces
// two exported predicates. waitReason returns (0, false) for THREE different
// configurations — settled, running, and work-ended-holding-everything — so a
// caller given only the reason cannot fill RenderView.Running. Both rows below
// have no wait reason and differ only in Running.
func TestRender_DistinguishesRunningFromReadyToAdvance(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (grant): %v", err)
	}

	running := q.Render(j)
	if !running.Running || running.Reason != 0 {
		t.Fatalf("mid-work: Running=%v Reason=%v, want true and no reason",
			running.Running, running.Reason)
	}

	// Fetching's work ends and Advance crosses it into Assessing, granting
	// the compute slot Assessing needs (Fetching itself needs none — §3.4).
	// It is running there too.
	if err := j.SetNext(job.Assessing); err != nil {
		t.Fatalf("SetNext(Assessing): %v", err)
	}
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (to Assessing): %v", err)
	}
	assessing := q.Render(j)
	if !assessing.Running || assessing.Reason != 0 {
		t.Fatalf("in Assessing: Running=%v Reason=%v, want true and no reason",
			assessing.Running, assessing.Reason)
	}

	// Assessing's work ends with a NeedsMore verdict: it demotes back to
	// Fetching, which needs only the lease Assessing already holds (§3.4) —
	// so it waits on nothing, but it is no longer running, because its work
	// has ended. This is TestWaitReason_UsesTheNextStateWhenWorkHasEnded's
	// scenario (queue_test.go), reached here through Render instead of a
	// literal Snapshot.
	if err := j.SetNext(job.Fetching); err != nil {
		t.Fatalf("SetNext(Fetching): %v", err)
	}
	ended := q.Render(j)
	if ended.Running {
		t.Error("work ended: Running = true, want false — a job waiting to move " +
			"is not running (§3.4)")
	}
	if ended.Reason != 0 {
		t.Errorf("work ended: Reason = %v, want none — it holds what Fetching "+
			"needs (its lease) and is simply awaiting a tick", ended.Reason)
	}
	if ended.Next != job.Fetching {
		t.Errorf("work ended: Next = %v, want Fetching", ended.Next)
	}
}

// TestRenderAll_TakesTheQueueLock proves RenderAll locks AT ALL, and only
// that. It cannot prove the lock is taken ONCE: an implementation that locked
// and unlocked around each row is equally race-free, returns the same rows,
// and passes this test unchanged — verified by mutating render.go to lock
// per row, which leaves this test reporting ok under -race while
// TestRenderAll_LocksOnceOutsideTheRowLoop (lock_enumeration_test.go) fails.
// The single-span property is invisible at runtime and plain in the source,
// so it is pinned statically there rather than here; an earlier name for this
// test said "Once" and claimed both halves.
//
// It departs from the task brief's draft in
// one respect: the brief's version leaves both jobs at Fetching and loops
// q.Advance(b) 200 times. Fetching needsLease but not needsSlot (§3.4), and
// q.holds only touches q.slots.held when the position needsSlot — so a job
// that never leaves Fetching never causes RenderAll's read side to touch
// q.slots.held at all, and the mutation this test exists to catch (removing
// RenderAll's q.mu.Lock/Unlock pair) produced no race across repeated runs
// with the brief's exact scenario; see task-1-report.md's Step 7 section for
// the observed (non-)failures. Moving both jobs to Assessing first, which
// DOES needSlot, and using a single concurrent Park(b) — TestRender_TakesTheQueueLock's
// own proven pattern (advance_test.go) — gives q.slots.held a genuine
// concurrent reader (RenderAll's loop) and writer (Park's releaseFor) on the
// SAME map, which is what the race detector actually needs to catch an
// unlocked RenderAll.
func TestRenderAll_TakesTheQueueLock(t *testing.T) {
	q := New(2, 2, testClock, &stubWorkers{})
	a := job.New("a", "n", job.Policy{})
	b := job.New("b", "n", job.Policy{})
	mustAdvanceTo(t, q, a, job.Assessing)
	mustAdvanceTo(t, q, b, job.Assessing)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := q.Park(b); err != nil {
			t.Errorf("Park(b): %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			got := q.RenderAll([]*job.Job{a, b})
			if len(got) != 2 {
				t.Errorf("RenderAll returned %d rows, want 2", len(got))
			}
		}
	}()
	wg.Wait()
}

func TestRenderAll_MatchesRenderPerJob(t *testing.T) {
	q := New(2, 2, testClock, &stubWorkers{})
	a := job.New("a", "n", job.Policy{})
	b := job.New("b", "n", job.Policy{})
	if err := a.BeginAttempt(q.now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}

	all := q.RenderAll([]*job.Job{a, b})
	if len(all) != 2 {
		t.Fatalf("RenderAll returned %d rows, want 2", len(all))
	}
	for i, j := range []*job.Job{a, b} {
		if want := q.Render(j); all[i] != want {
			t.Errorf("row %d = %+v, want %+v — RenderAll and Render must compute the same view", i, all[i], want)
		}
	}
}

func TestRenderAll_EmptyInputReturnsEmptyNonNil(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	got := q.RenderAll(nil)
	if got == nil {
		t.Fatal("RenderAll(nil) = nil, want an empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("RenderAll(nil) has %d rows, want 0", len(got))
	}
}

// TestRender_FillsHolds pins that renderLocked fills job.RenderView.Holds
// from q.holds(j.ID(), s) — the addition beyond the task brief. A job at
// Fetching after its lease has been granted holds everything Fetching
// requires (§3.4: Fetching needs only the lease), so Holds must read true; a
// freshly-begun attempt before any Advance grants a lease holds nothing, so
// Holds must read false.
func TestRender_FillsHolds(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil { // begins the attempt; no lease yet
		t.Fatalf("Advance (begin): %v", err)
	}
	before := q.Render(j)
	if before.Holds {
		t.Error("Holds = true before any lease is granted, want false")
	}

	if err := q.Advance(j); err != nil { // grants the Fetching lease
		t.Fatalf("Advance (grant): %v", err)
	}
	after := q.Render(j)
	if !after.Holds {
		t.Error("Holds = false once the Fetching lease is granted, want true — " +
			"Fetching needs only the lease (§3.4) and the job now has it")
	}
}

// TestRender_ReportsTheGateReason pins that gates reach the view.
func TestRender_ReportsTheGateReason(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	if err := q.Advance(j); err != nil {
		t.Fatalf("Advance (begin): %v", err)
	}
	if err := j.SetIntent(job.IntentPause); err != nil {
		t.Fatalf("SetIntent: %v", err)
	}
	v := q.Render(j)
	if v.Reason != job.UserPaused {
		t.Errorf("Reason = %v, want UserPaused", v.Reason)
	}
	if v.Intent != job.IntentPause {
		t.Errorf("Intent = %v, want IntentPause", v.Intent)
	}
}

// TestRenderLocked_AssumesTheCallerAlreadyHoldsTheLock pins renderLocked's own
// contract directly, in TestParkLocked_AssumesTheCallerAlreadyHoldsTheLock's
// shape (advance_test.go): unlike Render and RenderAll, renderLocked must NOT
// take q.mu itself — both of its callers already hold it when they call in,
// and a renderLocked that tried to lock again would deadlock there. Calling
// it here with q.mu already held, from the test goroutine itself, is exactly
// that shape.
//
// The assertions are concrete field values known independently from
// mustAdvanceTo's own contract (a job at Assessing that mustAdvanceTo landed
// there holds its lease and has no pending Next), not a comparison against
// q.Render(j)'s own output. Render calls renderLocked internally, so a
// renderLocked-vs-Render comparison is tautological: a renderLocked mutated
// to always return a zero job.RenderView would make Render return the same
// zero value, and the two sides would agree with each other while both are
// wrong. Asserting the actual expected shape catches that; a
// renderLocked-vs-Render comparison, tried first here, did not — verified
// below.
func TestRenderLocked_AssumesTheCallerAlreadyHoldsTheLock(t *testing.T) {
	q := New(1, 1, testClock, &stubWorkers{})
	j := job.New("j1", "n", job.Policy{})
	mustAdvanceTo(t, q, j, job.Assessing)

	q.mu.Lock()
	got := q.renderLocked(j)
	q.mu.Unlock()

	if got.State != job.Assessing {
		t.Errorf("State = %v, want Assessing", got.State)
	}
	if got.Next != job.StateUnset {
		t.Errorf("Next = %v, want StateUnset — mustAdvanceTo left no pending transition", got.Next)
	}
	if !got.Running {
		t.Error("Running = false, want true — the job holds what Assessing requires and Next is unset")
	}
	if !got.Holds {
		t.Error("Holds = false, want true — Assessing needs BOTH a lease and a compute slot (needsLease and needsSlot are each true for it), and mustAdvanceTo granted both")
	}
	if got.Intent != job.IntentRun {
		t.Errorf("Intent = %v, want IntentRun — nothing asked for pause or cancel", got.Intent)
	}
	if got.Reason != job.NoLease {
		t.Errorf("Reason = %v, want NoLease's zero value — Reason is meaningless while Running", got.Reason)
	}
}

// TestRender_NeverRunJobHoldsNothing pins that a job which has never started
// reports Holds=false. It is a regression pin, not a tautology: holds() asks
// whether the job has everything its CURRENT position requires, and at
// StateUnset both requirement guards are vacuous — needsLease and needsSlot
// are false there — so the function fell through to true and reported that a
// job owning zero pool resources held everything it needed.
//
// The consumer that suffered is RenderView.Holds. internal/dispatch's
// reconcileResidency hydrates on `v.Holds && !resident`, so a paused,
// never-started job had its manifest read into memory while holding nothing,
// and a failed read then tried to settle a job with no open attempt.
func TestRender_NeverRunJobHoldsNothing(t *testing.T) {
	q := New(2, 2, testClock, &stubWorkers{})
	j := job.New("never", "n", job.Policy{})

	v := q.Render(j)

	if v.State != job.StateUnset {
		t.Fatalf("setup: State = %v, want StateUnset", v.State)
	}
	if v.Holds {
		t.Error("Holds = true for a job that has never run — it owns no lease " +
			"and no slot, so there is nothing for it to hold")
	}
	if v.Running {
		t.Error("Running = true for a job that has never run")
	}
}
