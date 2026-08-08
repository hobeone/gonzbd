package app

import (
	"testing"
	"time"
)

// A closed internalFileComplete must terminate both of its consumers, not spin
// them.
//
// Nothing in production closes that channel, so neither `, ok` guard is
// reachable outside a test. That is exactly why they are guarded and why these
// tests exist: "no file anywhere calls close()" is a whole-program invariant
// that cannot be checked at the receive site, so a future close has to fail
// loudly here rather than silently pinning a core. See docs/go-standards.md
// § Concurrency & Locking.
//
// Without the guards a closed channel is permanently ready, so the receive wins
// every iteration — in drainCompletions the `default:` arm is never selected,
// and in watchCompletions the loop never blocks. Either way the loop runs
// forever handing zero-value FileComplete events to handleFileComplete; a zero
// event has an empty JobID, so each iteration reaches logQueueWriteFailure's
// Debug call and allocates, reproducing #336 exactly. These tests would then
// hang rather than fail fast.
//
// New(cfg, nil) skips the SQLite store entirely — neither loop touches the
// queue or history once the first receive reports !ok.
func newClosedCompletionsApp(t *testing.T) *Application {
	t.Helper()
	application, err := New(testConfigInternal(t, t.TempDir()), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	close(application.internalFileComplete)
	return application
}

func TestDrainCompletionsTerminatesWhenChannelClosed(t *testing.T) {
	application := newClosedCompletionsApp(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		application.drainCompletions(t.Context())
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("drainCompletions did not return on a closed channel; " +
			"without the `, ok` guard the receive is permanently ready, `default` never fires, and the loop spins")
	}
}

func TestWatchCompletionsTerminatesWhenChannelClosed(t *testing.T) {
	application := newClosedCompletionsApp(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		application.watchCompletions(t.Context())
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("watchCompletions did not return on a closed channel; " +
			"without the `, ok` guard the receive is permanently ready and the loop spins")
	}
}
