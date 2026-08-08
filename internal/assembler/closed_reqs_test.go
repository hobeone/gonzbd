package assembler

import (
	"testing"
	"time"
)

// Closing reqs must terminate the worker, not spin it.
//
// Nothing in production closes reqs, so neither guard is reachable outside a
// test. That is exactly why the test exists: the invariant is whole-program
// ("no file anywhere calls close(a.reqs)") and cannot be checked at the receive
// site, so a future close must fail loudly here rather than silently pinning a
// core. See docs/go-standards.md § Concurrency & Locking.
//
// Without the `, ok` guards a closed reqs is permanently ready, so the drain
// loop's `default:` arm is never selected and the worker loops forever
// dispatching zero-value WriteRequests — Stop() would then never return and
// this test times out.
//
// The worker's outer select races between the reqs arm and the stopCh arm, and
// only the stopCh arm reaches the inner drain loop. Repeat so both paths are
// exercised; a single pass could take either.
func TestWorkerTerminatesWhenReqsClosed(t *testing.T) {
	const attempts = 30

	for i := range attempts {
		a := New(makeOpts(t.TempDir(), nil), nil)
		if err := a.Start(t.Context()); err != nil {
			t.Fatalf("attempt %d: Start: %v", i, err)
		}

		close(a.reqs)

		done := make(chan error, 1)
		go func() { done <- a.Stop() }()

		select {
		case <-done:
			// Terminated. Whether it exited via the outer receive or the inner
			// drain loop is not asserted — either is a correct shutdown.
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: worker did not terminate after reqs was closed; "+
				"a missing `, ok` guard makes the closed channel permanently ready and spins the loop", i)
		}
	}
}
