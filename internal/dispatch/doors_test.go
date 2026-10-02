package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

func TestCancel_NoJobReturnsError(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Cancel("missing"); err == nil {
		t.Fatal("Cancel(missing) returned nil, want an error")
	}
}

func TestCancel_LatchesAndKicksForARegisteredJob(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	<-d.wake // Add's own kick

	if err := d.Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if j.Snapshot().Intent != job.IntentCancel {
		t.Error("Cancel did not latch IntentCancel on the job")
	}
	select {
	case <-d.wake:
	default:
		t.Error("Cancel did not kick the tick")
	}
}

func TestCancelJob_NilJobReturnsErrNotFound(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.CancelJob(nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("CancelJob(nil) = %v, want ErrNotFound", err)
	}
}

// TestCancelJob_LeavesALaterInstanceAlone: a cancel carrying a removed
// instance does not latch the attempt since registered under the same ID,
// while one carrying the registered instance does.
func TestCancelJob_LeavesALaterInstanceAlone(t *testing.T) {
	d := newTestDispatcher(t)
	j1 := job.New("j1", "first", job.Policy{})
	if err := d.Add(context.Background(), j1, Header{}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}
	if err := d.Remove(context.Background(), "j1"); err != nil {
		t.Fatalf("Remove(j1): %v", err)
	}
	j2 := job.New("j1", "second", job.Policy{})
	if err := d.Add(context.Background(), j2, Header{}); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}

	if err := d.CancelJob(j1); !errors.Is(err, ErrNotFound) {
		t.Errorf("CancelJob(removed instance) = %v, want ErrNotFound", err)
	}
	if got := j2.Snapshot().Intent; got == job.IntentCancel {
		t.Fatal("CancelJob with the removed instance latched IntentCancel on the later one")
	}
	if err := d.CancelJob(j2); err != nil {
		t.Fatalf("CancelJob(registered instance): %v", err)
	}
	if got := j2.Snapshot().Intent; got != job.IntentCancel {
		t.Errorf("CancelJob with the registered instance left intent %v, want IntentCancel", got)
	}
}

func TestPauseResumePaused_DelegateToTheQueue(t *testing.T) {
	d := newTestDispatcher(t)

	if d.Paused() {
		t.Fatal("setup: dispatcher reports paused before Pause was called")
	}

	d.Pause()
	if !d.Paused() {
		t.Error("Paused() is false after Pause()")
	}
	select {
	case <-d.wake:
	default:
		t.Error("Pause did not kick the tick")
	}

	d.Resume()
	if d.Paused() {
		t.Error("Paused() is true after Resume()")
	}
	select {
	case <-d.wake:
	default:
		t.Error("Resume did not kick the tick")
	}
}

// TestLookupFor_MatchesByInstanceOnlyWhenAsked pins the identity rule the
// instance-scoped doors share: a nil expected matches whatever is registered
// under the ID, and a non-nil one matches only that exact instance.
func TestLookupFor_MatchesByInstanceOnlyWhenAsked(t *testing.T) {
	d := newTestDispatcher(t)
	registered := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), registered, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	other := job.New("j1", "n", job.Policy{})

	tests := []struct {
		name     string
		id       string
		expected *job.Job
		want     *job.Job
	}{
		{"unregistered ID", "missing", nil, nil},
		{"any instance", "j1", nil, registered},
		{"the registered instance", "j1", registered, registered},
		{"another instance under the same ID", "j1", other, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := d.lookupFor(tc.id, tc.expected)
			if got != tc.want || ok != (tc.want != nil) {
				t.Errorf("lookupFor(%q) = (%p, %v), want (%p, %v)", tc.id, got, ok, tc.want, tc.want != nil)
			}
		})
	}
}

// TestCancelFor_LatchesOnlyTheJobItFinds: a miss is ErrNotFound and latches
// nothing, and a hit latches IntentCancel on the registered job.
func TestCancelFor_LatchesOnlyTheJobItFinds(t *testing.T) {
	d := newTestDispatcher(t)
	registered := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), registered, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := d.cancelFor("missing", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancelFor(missing) = %v, want ErrNotFound", err)
	}
	if err := d.cancelFor("j1", job.New("j1", "n", job.Policy{})); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancelFor(another instance) = %v, want ErrNotFound", err)
	}
	if got := registered.Snapshot().Intent; got == job.IntentCancel {
		t.Fatal("a cancelFor that missed latched IntentCancel on the registered job")
	}
	if err := d.cancelFor("j1", nil); err != nil {
		t.Fatalf("cancelFor(j1): %v", err)
	}
	if got := registered.Snapshot().Intent; got != job.IntentCancel {
		t.Errorf("cancelFor(j1) left intent %v, want IntentCancel", got)
	}
}
