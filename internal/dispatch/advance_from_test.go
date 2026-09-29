package dispatch

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

func claimHeld(d *Dispatcher, id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.launched[id]
	return ok
}

// TestAdvanceFrom_LateReportLeavesTheNextStatesWorkerAlone pins the report
// against a job that has already moved on. A download-complete report that
// arrives again after the tick has moved the job to Assessing and launched it
// must not park that worker's resources or clear its claim; if it does, the
// next tick launches Assessing a second time.
func TestAdvanceFrom_LateReportLeavesTheNextStatesWorkerAlone(t *testing.T) {
	runner := &stateRunner{}
	d := newTestDispatcher(t, withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background()) // branch 1: BeginAttempt at Fetching
	d.tick(context.Background()) // branch 2 grants; launches Fetching

	if err := d.AdvanceFrom(j, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom: %v", err)
	}
	if claimHeld(d, j.ID()) {
		t.Fatal("the Fetching worker's claim survived its own report")
	}
	d.tick(context.Background()) // branch 3 moves to Assessing and launches it

	err := d.AdvanceFrom(j, job.Fetching, job.Assessing)
	if !errors.Is(err, ErrStaleReport) {
		t.Errorf("late AdvanceFrom = %v, want ErrStaleReport", err)
	}
	if !claimHeld(d, j.ID()) {
		t.Error("a late report cleared the Assessing worker's claim")
	}
	if v := d.q.Render(j); !v.Running {
		t.Errorf("a late report stripped the Assessing worker's resources: %+v", v)
	}

	d.tick(context.Background())
	if got, want := runner.ran(), []job.State{job.Fetching, job.Assessing}; !slices.Equal(got, want) {
		t.Errorf("runs = %v, want %v", got, want)
	}
}

// TestAdvanceFrom_UnlaunchedReportLaunchesTheNextStateOnce is the interleaving
// that double-launched: a report for a job whose Fetching worker holds no
// claim, as after a stall's resume or at startup, followed by a tick that
// launches Assessing before the report's tail lands. With one door there is
// no tail, and a repeat of the report is refused.
func TestAdvanceFrom_UnlaunchedReportLaunchesTheNextStateOnce(t *testing.T) {
	runner := &stateRunner{}
	d := newTestDispatcher(t, withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background()) // BeginAttempt at Fetching; nothing launched

	if err := d.AdvanceFrom(j, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom: %v", err)
	}
	d.tick(context.Background()) // moves to Assessing and launches it
	if err := d.AdvanceFrom(j, job.Fetching, job.Assessing); !errors.Is(err, ErrStaleReport) {
		t.Errorf("repeated AdvanceFrom = %v, want ErrStaleReport", err)
	}
	d.tick(context.Background())

	if got, want := runner.ran(), []job.State{job.Assessing}; !slices.Equal(got, want) {
		t.Errorf("runs = %v, want %v", got, want)
	}
}

// TestAdvanceFrom_RecordedNextIsStale pins the no-next clause: a job whose
// next is already recorded has had its report, so a second one must neither
// park it nor clear the claim it holds.
func TestAdvanceFrom_RecordedNextIsStale(t *testing.T) {
	d := newTestDispatcher(t, withRunner(&stateRunner{}))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background()) // launches Fetching
	if err := j.SetNext(job.Assessing); err != nil {
		t.Fatalf("SetNext: %v", err)
	}

	if err := d.AdvanceFrom(j, job.Fetching, job.Assessing); !errors.Is(err, ErrStaleReport) {
		t.Errorf("AdvanceFrom = %v, want ErrStaleReport", err)
	}
	if !claimHeld(d, j.ID()) {
		t.Error("a stale report cleared the claim")
	}
	if !j.HoldsLease() {
		t.Error("a stale report parked the job")
	}
}

// TestAdvanceFrom_RefusedVerdictSettlesFailed pins that a refused verdict ends
// the job rather than relaunching the state to report it again: the job
// settles Failed, its claim is cleared, and no later tick launches it.
func TestAdvanceFrom_RefusedVerdictSettlesFailed(t *testing.T) {
	runner := &stateRunner{}
	d := newTestDispatcher(t, withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background()) // launches Fetching

	err := d.AdvanceFrom(j, job.Fetching, job.Finalizing)
	if !errors.Is(err, job.ErrIllegalTransition) {
		t.Errorf("AdvanceFrom = %v, want the SetNext refusal", err)
	}
	if got := j.Snapshot().State.Outcome; got != job.OutcomeFailed {
		t.Errorf("outcome = %v, want Failed", got)
	}
	if claimHeld(d, j.ID()) {
		t.Error("the claim survived a report that ended the job")
	}
	d.tick(context.Background())
	if got, want := runner.ran(), []job.State{job.Fetching}; !slices.Equal(got, want) {
		t.Errorf("runs = %v, want %v", got, want)
	}
}

// TestAdvanceFrom_NoVerdictIsRefused pins that AdvanceFrom will not stand in
// for YieldedFrom: a StateUnset verdict is refused before anything moves.
func TestAdvanceFrom_NoVerdictIsRefused(t *testing.T) {
	d := newTestDispatcher(t, withRunner(&stateRunner{}))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background()) // launches Fetching

	if err := d.AdvanceFrom(j, job.Fetching, job.StateUnset); !errors.Is(err, job.ErrIllegalTransition) {
		t.Errorf("AdvanceFrom(StateUnset) = %v, want ErrIllegalTransition", err)
	}
	if !claimHeld(d, j.ID()) || !j.HoldsLease() {
		t.Error("a refused StateUnset verdict released the worker")
	}
}

// TestYieldedFrom_ReleasesOnlyTheNamedState pins YieldedFrom's scope: at from
// it parks and clears the claim without recording a verdict, and at any other
// state it returns ErrStaleReport and leaves that state's worker alone.
func TestYieldedFrom_ReleasesOnlyTheNamedState(t *testing.T) {
	runner := &stateRunner{}
	d := newTestDispatcher(t, withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background()) // launches Fetching

	if err := d.YieldedFrom(j, job.Assessing); !errors.Is(err, ErrStaleReport) {
		t.Errorf("YieldedFrom(Assessing) at Fetching = %v, want ErrStaleReport", err)
	}
	if !claimHeld(d, j.ID()) || !j.HoldsLease() {
		t.Fatal("a yield for another state released the Fetching worker")
	}

	if err := d.YieldedFrom(j, job.Fetching); err != nil {
		t.Fatalf("YieldedFrom(Fetching): %v", err)
	}
	if claimHeld(d, j.ID()) || j.HoldsLease() {
		t.Error("YieldedFrom at from left the claim or the lease")
	}
	if got := j.Snapshot().State.Next; got != job.StateUnset {
		t.Errorf("a yield recorded next %v", got)
	}
	if err := d.YieldedFrom(nil, job.Fetching); !errors.Is(err, ErrNotFound) {
		t.Errorf("YieldedFrom(nil) = %v, want ErrNotFound", err)
	}
}

// TestAdvanceFrom_OtherInstanceIsNotFound pins the instance check: a report
// carrying a removed instance must not touch the one registered since.
func TestAdvanceFrom_OtherInstanceIsNotFound(t *testing.T) {
	d := newTestDispatcher(t, withRunner(&stateRunner{}))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background()) // launches Fetching

	other := job.New("j1", "n", job.Policy{})
	if err := other.BeginAttempt(testClock()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if err := d.AdvanceFrom(other, job.Fetching, job.Assessing); !errors.Is(err, ErrNotFound) {
		t.Errorf("AdvanceFrom(other) = %v, want ErrNotFound", err)
	}
	if err := d.AdvanceFrom(nil, job.Fetching, job.Assessing); !errors.Is(err, ErrNotFound) {
		t.Errorf("AdvanceFrom(nil) = %v, want ErrNotFound", err)
	}
	if !claimHeld(d, j.ID()) || !j.HoldsLease() || j.Snapshot().State.Next != job.StateUnset {
		t.Errorf("a report for another instance touched the registered one: claim %v, lease %v, next %v",
			claimHeld(d, j.ID()), j.HoldsLease(), j.Snapshot().State.Next)
	}
}

// TestClearLaunchedFor_LeavesALaterInstancesClaim pins clearLaunchedFor's
// instance check directly: the claim under a reused ID belongs to the later
// instance, and a report for the removed one must not clear it.
func TestClearLaunchedFor_LeavesALaterInstancesClaim(t *testing.T) {
	d := newTestDispatcher(t)
	j1 := job.New("j1", "first", job.Policy{})
	if err := d.Add(context.Background(), j1, Header{}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}
	rm, ok := d.beginRemoval("j1")
	if !ok {
		t.Fatal("beginRemoval: job not registered")
	}
	rm.end()
	j2 := job.New("j1", "second", job.Policy{})
	if err := d.Add(context.Background(), j2, Header{}); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}
	if !d.claimLaunched("j1") {
		t.Fatal("claimLaunched(j2) = false, want true")
	}

	d.clearLaunchedFor(j1)
	if !claimHeld(d, "j1") {
		t.Error("clearLaunchedFor(removed instance) cleared the later instance's claim")
	}
	d.clearLaunchedFor(j2)
	if claimHeld(d, "j1") {
		t.Error("clearLaunchedFor(registered instance) left its claim")
	}
}

// TestDispatcherHandoff_NamesItsDoorAndRefusesStaleReports pins handoff, the
// shared body of AdvanceFrom and YieldedFrom, directly: every refusal names
// the door that was called, and a report for a state the job is not at
// returns ErrStaleReport without touching it.
func TestDispatcherHandoff_NamesItsDoorAndRefusesStaleReports(t *testing.T) {
	d := newTestDispatcher(t, withRunner(&stateRunner{}))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background()) // launches Fetching

	if err := d.handoff("Door", nil, job.Fetching, job.Assessing); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "Door") {
		t.Errorf("handoff(nil) = %v, want ErrNotFound naming Door", err)
	}
	err := d.handoff("Door", j, job.Repairing, job.Assessing)
	if !errors.Is(err, ErrStaleReport) || !strings.Contains(err.Error(), "Door(j1") {
		t.Errorf("handoff at the wrong state = %v, want ErrStaleReport naming Door", err)
	}
	if !claimHeld(d, j.ID()) || !j.HoldsLease() {
		t.Error("a stale handoff released the worker")
	}
}
