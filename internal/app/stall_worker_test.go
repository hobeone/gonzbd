package app

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// stateRecorder is a Runner that records each launch's state per job and
// never reports, so a launched worker stays live until the test reports for it.
type stateRecorder struct {
	mu   sync.Mutex
	runs map[string][]job.State
}

func (r *stateRecorder) Run(_ context.Context, id string, s job.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = make(map[string][]job.State)
	}
	r.runs[id] = append(r.runs[id], s)
}

func (r *stateRecorder) ran(id string) []job.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.runs[id])
}

type nopResidency struct{}

func (nopResidency) Hydrate(context.Context, string) error { return nil }
func (nopResidency) Evict(string)                          {}

// TestStall_LeavesALiveAssessingWorkerAlone pins Stall against a job that
// has left Fetching. The checkpoint still reaches an Assessing job's open
// handles, so a storage fault can stall one while its assess worker runs.
// Releasing that worker's slot let a second job take pool B's only slot
// while the first assess was still running, and releasing its claim let the
// resume launch a second assess of the same job.
func TestStall_LeavesALiveAssessingWorkerAlone(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	runner := &stateRecorder{}
	d := dispatch.New(2, 1, time.Hour, time.Now, &appWorkers{app: application},
		nopResidency{}, nopDispatchStore{}, runner)
	application.dispatcher = d
	application.pipeline.dispatcher = d
	application.runner.report = d

	ctx := t.Context()
	j1 := job.New("j1", "first", job.Policy{})
	j2 := job.New("j2", "second", job.Policy{})
	for _, j := range []*job.Job{j1, j2} {
		if err := d.Add(ctx, j, dispatch.Header{}); err != nil {
			t.Fatalf("Add(%s): %v", j.ID(), err)
		}
	}
	d.Tick(ctx) // both begin at Fetching
	d.Tick(ctx) // both granted a lease and launched at Fetching
	if err := d.AdvanceFrom(j1, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom(j1): %v", err)
	}
	d.Tick(ctx) // j1 takes the only slot and launches at Assessing
	if err := d.AdvanceFrom(j2, job.Fetching, job.Assessing); err != nil {
		t.Fatalf("AdvanceFrom(j2): %v", err)
	}
	d.Tick(ctx) // j2 waits for the slot

	application.Stall(j1.ID(), testFault("sync"))
	d.Tick(ctx)
	if got := runner.ran(j2.ID()); slices.Contains(got, job.Assessing) {
		t.Errorf("j2 launched at Assessing while j1's assess still ran on pool B's only slot: j2 runs = %v", got)
	}
	if row, _ := d.Row(j1.ID()); !row.View.Holds {
		t.Errorf("the stall took j1's resources from its live assess worker: %+v", row.View)
	}

	if err := d.ResumeJob(j1.ID()); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	d.Tick(ctx)
	if got, want := runner.ran(j1.ID()), []job.State{job.Fetching, job.Assessing}; !slices.Equal(got, want) {
		t.Errorf("j1 runs = %v, want %v", got, want)
	}
}
