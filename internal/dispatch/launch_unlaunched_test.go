package dispatch

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// idStateRunner records every Run as (id, state) and never reports an exit
// itself, so a test decides when each worker reports.
type idStateRunner struct {
	mu   sync.Mutex
	runs []string
}

func (r *idStateRunner) Run(_ context.Context, id string, s job.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, id+"@"+s.String())
}

func (r *idStateRunner) ran(id string, s job.State) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.runs, id+"@"+s.String())
}

// toExtractingReport drives j1 from Add to the point where its next Advance
// crosses it to Extracting and grants it the compute slot.
func toExtractingReport(t *testing.T, d *Dispatcher, j1 *job.Job) {
	t.Helper()
	ctx := context.Background()
	if err := d.Add(ctx, j1, Header{}); err != nil {
		t.Fatalf("Add j1: %v", err)
	}
	d.tick(ctx) // BeginAttempt at Fetching
	d.tick(ctx) // lease granted, Fetching worker launched
	if err := d.AdvanceFrom(j1, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom Fetching: %v", err)
	}
	d.tick(ctx) // to Assessing, slot granted, worker launched
	if err := d.AdvanceFrom(j1, job.Assessing, job.Extracting); err != nil {
		t.Fatalf("AdvanceFrom Assessing: %v", err)
	}
}

// TestLaunch_DeclinedLaunchReturnsTheGrant pins that a job Advance granted
// resources to, and launch then declined to start, gives those resources
// back. Each case lands between the grant and the start of j1's Extracting
// worker, with one compute slot: cancelling it, pausing it, or removing it
// leaves no worker to report, and before launch parked such a job its slot
// stayed with it, and after a removal with its ID, until a restart. The
// assertion is that a second job then gets that slot at Assessing.
//
// Each case runs in two windows: before launch's first check, where the tick
// would run evictCancelledNeverRun and reconcileResidency, and between that
// check and the claim.
func TestLaunch_DeclinedLaunchReturnsTheGrant(t *testing.T) {
	hits := []struct {
		name string
		hit  func(t *testing.T, d *Dispatcher, id string)
	}{
		{"cancel", func(t *testing.T, d *Dispatcher, id string) {
			t.Helper()
			if err := d.Cancel(id); err != nil {
				t.Errorf("Cancel: %v", err)
			}
		}},
		{"pause", func(t *testing.T, d *Dispatcher, id string) {
			t.Helper()
			if err := d.PauseJob(id); err != nil {
				t.Errorf("PauseJob: %v", err)
			}
		}},
		{"remove", func(t *testing.T, d *Dispatcher, id string) {
			t.Helper()
			if err := d.Remove(context.Background(), id); err != nil {
				t.Errorf("Remove: %v", err)
			}
		}},
	}
	for _, beforeClaim := range []bool{false, true} {
		window := "before first check"
		if beforeClaim {
			window = "before claim"
		}
		for _, h := range hits {
			t.Run(window+"/"+h.name, func(t *testing.T) {
				ctx := context.Background()
				runner := &idStateRunner{}
				d := newTestDispatcher(t, withRunner(runner), withCaps(1, 1))
				j1 := job.New("j1", "n", job.Policy{})
				toExtractingReport(t, d, j1)

				if beforeClaim {
					fired := false
					d.beforeClaim = func(id string) {
						if id != j1.ID() || fired {
							return
						}
						fired = true
						h.hit(t, d, id)
					}
					d.tick(ctx) // crosses to Extracting with the slot; launch runs the seam
					d.beforeClaim = nil
					if !fired {
						t.Fatalf("setup: launch never reached the claim for j1 at Extracting; runs = %v", runner.runs)
					}
				} else {
					// The tick's own order, with the hit between its Advance and
					// its launch.
					if err := d.q.Advance(j1); err != nil {
						t.Fatalf("Advance: %v", err)
					}
					h.hit(t, d, j1.ID())
					d.launch(j1)
				}
				if runner.ran(j1.ID(), job.Extracting) {
					t.Fatal("setup: j1's Extracting worker was launched despite the " + h.name)
				}
				// In the declining call itself, not a tick later: a removal
				// deregisters the job, and no later tick visits it.
				if v := d.q.Render(j1); v.Holds {
					t.Errorf("j1 still holds its grant after launch declined it for a %s: view %+v", h.name, v)
				}

				j2 := job.New("j2", "n", job.Policy{})
				if err := d.Add(ctx, j2, Header{}); err != nil {
					t.Fatalf("Add j2: %v", err)
				}
				d.tick(ctx) // BeginAttempt at Fetching
				d.tick(ctx) // lease granted, Fetching worker launched
				if err := d.AdvanceFrom(j2, job.Fetching, job.Assessing); err != nil {
					t.Fatalf("AdvanceFrom j2 Fetching: %v", err)
				}
				d.tick(ctx)
				d.tick(ctx)
				if !runner.ran(j2.ID(), job.Assessing) {
					t.Errorf("j2 never ran at Assessing after j1's Extracting launch was declined "+
						"by a %s: j1's compute slot was never returned (j1 view %+v)", h.name, d.q.Render(j1))
				}
				if h.name == "cancel" {
					if got := d.q.Render(j1).Outcome; got != job.OutcomeCancelled {
						t.Errorf("j1 outcome = %s after its declined launch was parked, want %s",
							got, job.OutcomeCancelled)
					}
				}
			})
		}
	}
}

// TestParkGrant_LogsARefusedPark pins that a park the lease pool refuses is
// reported rather than dropped. The refusal is the pool's identity audit,
// reached with a lease another dispatcher's pool issued.
func TestParkGrant_LogsARefusedPark(t *testing.T) {
	ctx := context.Background()
	other := newTestDispatcher(t, withRunner(&idStateRunner{}))
	j := job.New("j1", "n", job.Policy{})
	if err := other.Add(ctx, j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	other.tick(ctx)
	other.tick(ctx)
	if !j.HoldsLease() {
		t.Fatal("setup: j holds no lease")
	}

	var buf bytes.Buffer
	d := newTestDispatcher(t)
	d.log = captureLogger(&buf)
	d.parkGrant(j)
	if !strings.Contains(buf.String(), "failed to return the resources") {
		t.Errorf("a refused park was not logged; log = %q", buf.String())
	}
}

// TestLaunch_DeclinedLaunchLeavesALiveWorkersGrant pins the other side: a job
// whose worker is running keeps its resources when a cancel reaches launch's
// first check on a later tick. The claim is what tells the two apart.
func TestLaunch_DeclinedLaunchLeavesALiveWorkersGrant(t *testing.T) {
	ctx := context.Background()
	runner := &idStateRunner{}
	d := newTestDispatcher(t, withRunner(runner), withCaps(1, 1))
	j1 := job.New("j1", "n", job.Policy{})
	toExtractingReport(t, d, j1)
	d.tick(ctx)
	if !runner.ran(j1.ID(), job.Extracting) {
		t.Fatal("setup: j1's Extracting worker never launched")
	}

	if err := d.Cancel(j1.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	d.tick(ctx)
	d.tick(ctx)
	if v := d.q.Render(j1); !v.Running || !v.Holds {
		t.Errorf("j1's live Extracting worker lost its slot to a cancel: view %+v", v)
	}
}
