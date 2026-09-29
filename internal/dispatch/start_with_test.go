package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// verdictRow is a job persisted after its Fetching worker recorded a verdict:
// the one restored shape that a tick moves without any worker running.
func verdictRow() Persisted {
	return Persisted{
		ID:     "j1",
		Header: Header{Name: "j1"},
		State:  job.StateView{State: job.Fetching, Next: job.Assessing},
	}
}

func stateOf(d *Dispatcher, id string) job.StateView {
	j, ok := d.lookup(id)
	if !ok {
		return job.StateView{}
	}
	return j.Snapshot().State
}

// TestStartWith_RunsBeforeTheFirstTick pins that beforeFirstTick sees every
// restored job at its persisted position, and that no tick moves one while it
// runs.
//
// The window is what makes it discriminate. restore's registrations prime the
// wake, so a ticker launched before beforeFirstTick moves the job to Assessing
// within microseconds; half a second is far past that. The final wait is the
// fixture guard: without it the test would pass for a dispatcher that never
// ticked this job at all.
func TestStartWith_RunsBeforeTheFirstTick(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{verdictRow()})
	d := newTestDispatcher(t, withStore(st))

	var atHook job.StateView
	movedDuring := false
	err := d.StartWith(context.Background(), func(context.Context) error {
		atHook = stateOf(d, "j1")
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if stateOf(d, "j1").State != job.Fetching {
				movedDuring = true
				return nil
			}
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("StartWith: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	if atHook.State != job.Fetching || atHook.Next != job.Assessing {
		t.Errorf("beforeFirstTick saw j1 at %+v, want Fetching{next: Assessing} — the "+
			"step must see each job where it was persisted", atHook)
	}
	if movedDuring {
		t.Error("a tick moved j1 out of Fetching while beforeFirstTick was running")
	}

	deadline := time.Now().Add(5 * time.Second)
	for stateOf(d, "j1").State != job.Assessing {
		if time.Now().After(deadline) {
			t.Fatalf("j1 is at %+v after Start, want Assessing — the ticker never moved it, "+
				"so this test proves nothing about when it would have", stateOf(d, "j1"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStartWith_StepErrorFailsStart pins that a failed beforeFirstTick fails
// Start the way a failed restore does: the error comes back, no tick runs, and
// a later Stop returns rather than waiting on a ticker that never launched.
func TestStartWith_StepErrorFailsStart(t *testing.T) {
	st := &fakeStore{}
	st.seed([]Persisted{verdictRow()})
	d := newTestDispatcher(t, withStore(st))

	errStep := errors.New("step failed")
	err := d.StartWith(context.Background(), func(context.Context) error { return errStep })
	if !errors.Is(err, errStep) {
		t.Fatalf("StartWith = %v, want an error wrapping %v", err, errStep)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- d.Stop() }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after a failed StartWith; it is waiting on a ticker " +
			"that was never launched")
	}
	if got := stateOf(d, "j1"); got.State != job.Fetching {
		t.Errorf("j1 is at %+v after a failed StartWith, want Fetching — a tick ran", got)
	}
}
