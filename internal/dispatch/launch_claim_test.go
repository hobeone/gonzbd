package dispatch

import (
	"context"
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
	}{
		{"Finished", func(t *testing.T, d *Dispatcher, j *job.Job) {
			t.Helper()
			if err := d.Finished(j, job.OutcomeFailed); err != nil {
				t.Fatalf("Finished: %v", err)
			}
			if err := d.Retry(j.ID()); err != nil {
				t.Fatalf("Retry: %v", err)
			}
		}},
		{"Yielded", func(t *testing.T, d *Dispatcher, j *job.Job) {
			t.Helper()
			if err := d.Yielded(j); err != nil {
				t.Fatalf("Yielded: %v", err)
			}
		}},
		{"Stop", func(t *testing.T, d *Dispatcher, j *job.Job) {
			t.Helper()
			if err := d.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			d := newTestDispatcher(t, withRunner(runner))
			j := job.New("j1", "n", job.Policy{})
			if err := d.Add(j, Header{}); err != nil {
				t.Fatalf("Add: %v", err)
			}
			d.tick(context.Background())
			d.tick(context.Background())
			if !runner.started(j.ID()) {
				t.Fatal("setup: runner never started")
			}

			tc.exit(t, d, j)

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
