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

// TestCancelFor_LeavesALaterInstanceAlone: a cancel carrying a removed
// instance does not latch the attempt since registered under the same ID,
// while one carrying the registered instance does.
func TestCancelFor_LeavesALaterInstanceAlone(t *testing.T) {
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

	if err := d.CancelFor("j1", j1); !errors.Is(err, ErrNotFound) {
		t.Errorf("CancelFor(j1, removed instance) = %v, want ErrNotFound", err)
	}
	if got := j2.Snapshot().Intent; got == job.IntentCancel {
		t.Fatal("CancelFor with the removed instance latched IntentCancel on the later one")
	}
	if err := d.CancelFor("j1", j2); err != nil {
		t.Fatalf("CancelFor(j1, registered instance): %v", err)
	}
	if got := j2.Snapshot().Intent; got != job.IntentCancel {
		t.Errorf("CancelFor with the registered instance left intent %v, want IntentCancel", got)
	}
}

func TestRetry_NoJobReturnsError(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Retry("missing"); err == nil {
		t.Fatal("Retry(missing) returned nil, want an error")
	}
}

func TestRetry_ReopensASettledJobAndKicks(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(context.Background(), j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if err := d.Finished(j.ID(), job.OutcomeFailed); err != nil {
		t.Fatalf("Finished: %v", err)
	}
	<-d.wake // drain Finished's own kick

	if err := d.Retry(j.ID()); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if j.Snapshot().State.Outcome.IsSettled() {
		t.Error("Retry left the job settled — want a reopened attempt")
	}
	select {
	case <-d.wake:
	default:
		t.Error("Retry did not kick the tick")
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
