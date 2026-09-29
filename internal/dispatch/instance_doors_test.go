package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// laterInstance registers j1 under "j1", removes it, and registers j2 under the
// same ID, returning both.
func laterInstance(t *testing.T, d *Dispatcher) (j1, j2 *job.Job) {
	t.Helper()
	j1 = job.New("j1", "first", job.Policy{})
	if err := d.Add(context.Background(), j1, Header{}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}
	if err := d.Remove(context.Background(), "j1"); err != nil {
		t.Fatalf("Remove(j1): %v", err)
	}
	j2 = job.New("j1", "second", job.Policy{})
	if err := d.Add(context.Background(), j2, Header{}); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}
	return j1, j2
}

func TestRemoveJob_NilJobReturnsErrNotFound(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.RemoveJob(context.Background(), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveJob(nil) = %v, want ErrNotFound", err)
	}
}

// TestRemoveJob_LeavesALaterInstanceAlone: a removal carrying a removed
// instance neither cancels nor deregisters the attempt since registered under
// the same ID, while one carrying the registered instance removes it.
func TestRemoveJob_LeavesALaterInstanceAlone(t *testing.T) {
	d := newTestDispatcher(t)
	j1, j2 := laterInstance(t, d)

	if err := d.RemoveJob(context.Background(), j1); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveJob(removed instance) = %v, want ErrNotFound", err)
	}
	if got, ok := d.Job("j1"); !ok || got != j2 {
		t.Fatalf("Job(j1) = (%p, %v) after RemoveJob with the removed instance, want the later instance %p", got, ok, j2)
	}
	if got := j2.Snapshot().Intent; got == job.IntentCancel {
		t.Fatal("RemoveJob with the removed instance latched IntentCancel on the later one")
	}
	if d.removing["j1"] != 0 {
		t.Errorf("removing[j1] = %d after a refused RemoveJob, want 0", d.removing["j1"])
	}

	if err := d.RemoveJob(context.Background(), j2); err != nil {
		t.Fatalf("RemoveJob(registered instance): %v", err)
	}
	if _, ok := d.Job("j1"); ok {
		t.Error("RemoveJob with the registered instance left it registered")
	}
}

// TestInstanceBoundHelpers_ExpectedSelectsTheInstance: the helpers behind the
// by-ID and instance-bound doors act on whatever holds the ID when expected is
// nil, and only on expected otherwise.
func TestInstanceBoundHelpers_ExpectedSelectsTheInstance(t *testing.T) {
	cases := []struct {
		name     string
		expected func(j1, j2 *job.Job) *job.Job
		want     bool
	}{
		{"by ID", func(_, _ *job.Job) *job.Job { return nil }, true},
		{"registered instance", func(_, j2 *job.Job) *job.Job { return j2 }, true},
		{"removed instance", func(j1, _ *job.Job) *job.Job { return j1 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("beginRemovalFor", func(t *testing.T) {
				d := newTestDispatcher(t)
				j1, j2 := laterInstance(t, d)
				rm, ok := d.beginRemovalFor("j1", tc.expected(j1, j2))
				if ok != tc.want {
					t.Fatalf("beginRemovalFor ok = %v, want %v", ok, tc.want)
				}
				if got := d.removing["j1"]; (got == 1) != tc.want {
					t.Errorf("removing[j1] = %d, want a marker only when ok", got)
				}
				rm.abort()
			})
			t.Run("occupyFor", func(t *testing.T) {
				d := newTestDispatcher(t)
				j1, j2 := laterInstance(t, d)
				ran := false
				err := d.occupyFor(context.Background(), "j1", tc.expected(j1, j2), func(context.Context) { ran = true })
				if ran != tc.want || (err == nil) != tc.want {
					t.Errorf("occupyFor ran = %v, err = %v; want ran %v", ran, err, tc.want)
				}
			})
			t.Run("removeFor", func(t *testing.T) {
				d := newTestDispatcher(t)
				j1, j2 := laterInstance(t, d)
				err := d.removeFor(context.Background(), "j1", tc.expected(j1, j2))
				if (err == nil) != tc.want {
					t.Errorf("removeFor err = %v, want success %v", err, tc.want)
				}
				if _, ok := d.Job("j1"); ok == tc.want {
					t.Errorf("Job(j1) registered = %v after removeFor, want %v", ok, !tc.want)
				}
			})
		})
	}
}

func TestOccupyJob_NilJobReturnsErrNotFound(t *testing.T) {
	d := newTestDispatcher(t)
	err := d.OccupyJob(context.Background(), nil, func(context.Context) {
		t.Error("OccupyJob(nil) ran its callback")
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("OccupyJob(nil) = %v, want ErrNotFound", err)
	}
}

// TestOccupyJob_LeavesALaterInstanceAlone: occupying with a removed instance
// does not run the callback on the attempt since registered under the same ID,
// while occupying with the registered instance does.
func TestOccupyJob_LeavesALaterInstanceAlone(t *testing.T) {
	d := newTestDispatcher(t)
	j1, j2 := laterInstance(t, d)

	err := d.OccupyJob(context.Background(), j1, func(context.Context) {
		t.Error("OccupyJob with the removed instance ran its callback on the later one")
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("OccupyJob(removed instance) = %v, want ErrNotFound", err)
	}

	ran := false
	if err := d.OccupyJob(context.Background(), j2, func(context.Context) { ran = true }); err != nil {
		t.Fatalf("OccupyJob(registered instance): %v", err)
	}
	if !ran {
		t.Error("OccupyJob with the registered instance did not run its callback")
	}
}
