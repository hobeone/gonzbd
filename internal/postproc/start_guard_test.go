package postproc

import (
	"errors"
	"runtime"
	"testing"
	"time"
)

// TestStart_DoubleCallReturnsError verifies that calling Start twice
// returns ErrAlreadyStarted without spawning a second worker goroutine.
// This is the regression test for M25.
func TestStart_DoubleCallReturnsError(t *testing.T) {
	before := runtime.NumGoroutine()

	p := New(Options{})
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("first Start: %v", err)
	}

	// Give the worker goroutine time to spin up.
	time.Sleep(20 * time.Millisecond)
	afterFirst := runtime.NumGoroutine()

	// Second call must fail.
	err := p.Start(t.Context())
	if err == nil {
		t.Fatal("second Start should return error")
	}
	if !errors.Is(err, ErrAlreadyStarted) {
		t.Errorf("error = %v, want ErrAlreadyStarted", err)
	}

	// Verify no extra goroutine was spawned.
	time.Sleep(20 * time.Millisecond)
	afterSecond := runtime.NumGoroutine()
	if afterSecond > afterFirst {
		t.Errorf("goroutine leak: %d after first Start, %d after second Start",
			afterFirst, afterSecond)
	}

	// Clean up.
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	afterStop := runtime.NumGoroutine()
	if afterStop > before+1 { // allow some slack for test runtime goroutines
		t.Errorf("goroutine leak after Stop: %d before, %d after", before, afterStop)
	}
}

// TestStart_ProcessesJobsAfterGuard verifies that the started guard does not
// interfere with normal processing. Jobs submitted after a successful Start
// should complete normally.
func TestStart_ProcessesJobsAfterGuard(t *testing.T) {
	stage := newRecordStage("s")
	done := make(chan struct{})
	p := New(Options{
		Stages:    []Stage{stage},
		OnJobDone: func(_ *Job) { close(done) },
	})

	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	// Second Start returns error but does not break processing.
	if err := p.Start(t.Context()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start: got %v, want ErrAlreadyStarted", err)
	}

	p.Process(makeJob(t, "test-job"))

	select {
	case <-done:
		// Good.
	case <-time.After(3 * time.Second):
		t.Fatal("job was not processed within timeout")
	}

	if stage.CallCount() != 1 {
		t.Errorf("stage called %d times, want 1", stage.CallCount())
	}
}
