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
