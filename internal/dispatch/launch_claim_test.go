package dispatch

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestWorkerExit_ClearsTheLaunchedClaimSoALaterTickCanRelaunch is the
// stranding-class regression: every one of Finished, Yielded and Stop is a
// worker-exit path, and each must clear the job's launched claim on every
// return, not only on its happy path — a claim left set after an exit makes
// claimLaunched refuse the job forever, and no later tick can ever start it
// again. Table-driven across the three sites rather than one test per site,
// because the property under test ("an exit path clears the claim") is one
// property, not three.
func TestWorkerExit_ClearsTheLaunchedClaimSoALaterTickCanRelaunch(t *testing.T) {
	tests := []struct {
		name string
		exit func(t *testing.T, d *Dispatcher, j *job.Job)
		// postExit runs immediately after exit, before the relaunch ticks
		// below, for a check that only makes sense at that instant. Only
		// Stop needs one: unlike Finished and Yielded, whose residency
		// cleanup runs a tick later through reconcileResidency, Stop's own
		// sweep is the only place that clears the job's residency
		// bookkeeping, so the property belongs right after Stop returns.
		postExit func(t *testing.T, d *Dispatcher, j *job.Job)
	}{
		{"Finished", func(t *testing.T, d *Dispatcher, j *job.Job) {
			t.Helper()
			if err := d.Finished(j.ID(), job.OutcomeFailed); err != nil {
				t.Fatalf("Finished: %v", err)
			}
			if err := d.Retry(j.ID()); err != nil {
				t.Fatalf("Retry: %v", err)
			}
		}, nil},
		{"Yielded", func(t *testing.T, d *Dispatcher, j *job.Job) {
			t.Helper()
			if err := d.Yielded(j.ID()); err != nil {
				t.Fatalf("Yielded: %v", err)
			}
		}, nil},
		{"Stop", func(t *testing.T, d *Dispatcher, j *job.Job) {
			t.Helper()
			if err := d.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}
		}, func(t *testing.T, d *Dispatcher, j *job.Job) {
			t.Helper()
			// Stop evicts the manifest (d.res.Evict) but must also clear
			// d.resident, the dispatcher's own bookkeeping of what
			// Residency believes is loaded. Without that second clear, a
			// later tick's reconcileResidency finds v.Holds true (the job
			// re-granted resources) and d.isResident already true, takes
			// NEITHER branch, and never re-hydrates: the job launches
			// against a manifest that Stop already evicted from disk.
			if d.isResident(j.ID()) {
				t.Error("job still marked resident after Stop — Stop evicted the manifest via Residency.Evict but left d.resident set, so a later tick will never re-hydrate it")
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			d := newTestDispatcher(t, withRunner(runner))
			j := job.New("j1", "n", job.Policy{})
			if err := d.Add(context.Background(), j, Header{}); err != nil {
				t.Fatalf("Add: %v", err)
			}
			d.tick(context.Background())
			d.tick(context.Background())
			if !runner.started(j.ID()) {
				t.Fatal("setup: runner never started")
			}

			tc.exit(t, d, j)
			if tc.postExit != nil {
				tc.postExit(t, d, j)
			}

			// Reset the runner's record so the assertion below can only pass
			// by observing a FRESH Run call made after the exit, not the one
			// from setup above.
			runner.mu.Lock()
			runner.seen = map[string]bool{}
			runner.mu.Unlock()

			d.tick(context.Background())
			d.tick(context.Background())

			if !runner.started(j.ID()) {
				t.Error("job never relaunched after exiting — the launched claim was stranded")
			}
		})
	}
}

// stateRunner is a Runner that records the state each Run was handed and never
// reports an exit itself, like the app's Fetching runner, whose exit report
// comes from the completion path rather than from the goroutine Run starts.
type stateRunner struct {
	mu   sync.Mutex
	runs []job.State
}

func (r *stateRunner) Run(_ context.Context, _ string, s job.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, s)
}

func (r *stateRunner) ran() []job.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.runs)
}

// TestLaunch_ReportBeforeClaimLeavesNoStrandedClaim pins launch against an
// exit report that lands after its Running check and before its claim. The
// report is the one a completed download makes, AdvanceFrom(Fetching,
// Assessing), which parks the job and clears a claim that does not exist yet.
// A launch that claims anyway starts a Fetching worker for a job that has
// already left Fetching; nothing will report for it, so the claim is never
// cleared and the job is never launched at Assessing.
func TestLaunch_ReportBeforeClaimLeavesNoStrandedClaim(t *testing.T) {
	runner := &stateRunner{}
	d := newTestDispatcher(t, withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background()) // branch 1: BeginAttempt at Fetching

	fired := false
	d.beforeClaim = func(id string) {
		if fired {
			return
		}
		fired = true
		if err := d.AdvanceFrom(j, job.Fetching, job.Assessing); err != nil {
			t.Errorf("AdvanceFrom(%s): %v", id, err)
		}
	}
	d.tick(context.Background()) // branch 2 grants the lease; launch runs the seam
	if !fired {
		t.Fatal("setup: launch never reached the claim for the Fetching job")
	}

	d.mu.Lock()
	_, claimed := d.launched[j.ID()]
	d.mu.Unlock()
	if claimed {
		t.Errorf("launch claim held for %s after launch, with no worker that will ever report; runs = %v", j.ID(), runner.ran())
	}

	d.tick(context.Background()) // branch 3 moves to Assessing and launches
	if got := runner.ran(); !slices.Contains(got, job.Assessing) {
		t.Errorf("job never launched at Assessing: runs = %v, state = %+v", got, j.State())
	}
}
