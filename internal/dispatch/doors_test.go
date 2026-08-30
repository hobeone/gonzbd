package dispatch

import (
	"context"
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
	if err := d.Add(j, Header{}); err != nil {
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

func TestRetry_NoJobReturnsError(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Retry("missing"); err == nil {
		t.Fatal("Retry(missing) returned nil, want an error")
	}
}

func TestRetry_ReopensASettledJobAndKicks(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
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
