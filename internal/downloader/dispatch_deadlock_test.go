package downloader

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestDispatchPass_ExhaustedEmitsDoNotBlockQueueWriters is the regression
// guard for the B.2 deadlock.
//
// Before the fix, tryDispatch emitted ErrNoServersLeft inline while
// holding locks. If the completions channel was full,
// the dispatcher blocked forever — and so did any goroutine trying to
// pause a job or modify state.
//
// The test pins the buffer at 1, lets exhausted emits fill it with no
// consumer draining, then asserts a pause call (Dispatcher.PauseJob)
// still completes promptly.
func TestDispatchPass_ExhaustedEmitsDoNotBlockQueueWriters(t *testing.T) {
	ms := newMockNNTP(t)
	// No articles added — every BODY request gets 430.

	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, []string{"a@h", "b@h", "c@h"})
	addTestJob(t, disp, j, m)

	srv := testServer(t, "only", ms.addr)
	d := New(disp, []*Server{srv}, nil, Options{CompletionsBuffer: 1}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Drain exactly one completion so subsequent dispatch passes hit the
	// "no eligible servers" path while the buffer is full again. We read
	// one item and then stop reading entirely — the dispatcher will end
	// up blocked on emitResult for every remaining exhausted article.
	select {
	case <-d.Completions():
	case <-time.After(2 * time.Second):
		t.Fatalf("no completion received; downloader may be stuck")
	}

	// Race-reproduction window (intentional, NOT a synchronization sleep):
	// give the dispatcher time to refill the cap-1 completions buffer and block
	// on the next exhausted emit. The assertion below is guarded by its own 2s timeout.
	time.Sleep(200 * time.Millisecond)

	// A writer must make progress even while the dispatcher is
	// blocked on a full completions channel.
	done := make(chan error, 1)
	go func() { done <- disp.PauseJob(j.ID()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("Pause returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("disp.PauseJob starved by downloader holding lock — B.2 regression")
	}

	// Sanity: job is now paused.
	if j, ok := disp.Job(j.ID()); ok && j.Intent() != job.IntentPause {
		t.Logf("job intent after Pause: %v", j.Intent())
	}
}
