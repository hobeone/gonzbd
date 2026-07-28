package postproc

import (
	"sync"
	"testing"
)

// Start's wg.Go (i.e. wg.Add(1)) and Stop's wg.Wait were unsynchronised with
// respect to each other. sync.WaitGroup requires that "calls with a positive
// delta that start when the counter is zero must happen before a Wait", so a
// concurrent first Start and Stop violated the contract directly.
//
// TestStartStop_Race in start_guard_test.go reaches this window too, but only
// probabilistically: it sleeps 1ms per iteration and, because Stop never
// resets started, only its first Start ever calls wg.Go. It needs -count=200
// to surface.
//
// This test drives the window directly instead. Each iteration uses a fresh
// PostProcessor, so the WaitGroup counter is genuinely zero when the two
// goroutines are released together, and a single -race run is enough.
func TestStartStop_ConcurrentFirstStartAndStop(t *testing.T) {
	for range 200 {
		p := New(Options{})

		release := make(chan struct{})
		var pair sync.WaitGroup
		pair.Add(2)

		go func() {
			defer pair.Done()
			<-release
			_ = p.Start(t.Context())
		}()
		go func() {
			defer pair.Done()
			<-release
			_ = p.Stop()
		}()

		close(release)
		pair.Wait()

		// Whichever order the two landed in, the worker must be shut down
		// before the next iteration: main_test.go runs goleak.VerifyTestMain,
		// so a surviving worker fails the package.
		if err := p.Stop(); err != nil {
			t.Fatalf("final Stop: %v", err)
		}
	}
}

// Stop must remain safe and idempotent on a processor that was never started:
// with no worker there has been no wg.Add, so Stop must not Wait on a
// WaitGroup a later Start could still increment.
func TestStop_BeforeStartIsSafe(t *testing.T) {
	p := New(Options{})

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("second Stop before Start: %v", err)
	}

	// A Start after those Stops must still bring up a worker that the final
	// Stop can shut down cleanly.
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop after Start: %v", err)
	}
}
