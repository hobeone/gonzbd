package downloader

import (
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// TestDispatchPass_ExhaustedEmitsDoNotBlockQueueWriters is the regression
// guard for the B.2 deadlock.
//
// Before the fix, tryDispatch emitted ErrNoServersLeft inline while
// holding both tryMu and the queue RLock taken by
// Queue.ForEachUnfinishedArticle. If the completions channel was full,
// the dispatcher blocked forever — and so did any goroutine trying to
// take the queue write lock (e.g. the pipeline consumer wanting to mark
// an article failed), because the RLock was still held.
//
// The test pins the buffer at 1, lets exhausted emits fill it with no
// consumer draining, then asserts a queue write-lock-requiring call
// (Queue.Pause) still completes promptly. A pre-B.2 build hangs here.
func TestDispatchPass_ExhaustedEmitsDoNotBlockQueueWriters(t *testing.T) {
	ms := newMockNNTP(t)
	// No articles added — every BODY request gets 430.

	q := queue.New()
	job := makeJobWithArticles(t, []string{"a@h", "b@h", "c@h"})
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}

	srv := testServer(t, "only", ms.addr)
	d := New(q, []*Server{srv}, Options{CompletionsBuffer: 1}, nil)
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

	// Give the dispatcher a moment to fill the buffer again and attempt
	// further exhausted emits (which now block).
	time.Sleep(200 * time.Millisecond)

	// A queue writer must make progress even while the dispatcher is
	// blocked on a full completions channel. Pre-B.2 this Pause call
	// deadlocks because the dispatcher holds the queue RLock.
	done := make(chan error, 1)
	go func() { done <- q.Pause(job.ID) }()

	select {
	case err := <-done:
		if err != nil {
			// Expected behaviour is successful pause; Pause may return
			// "already paused" on racy passes — both are non-failure.
			t.Logf("Pause returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("queue.Pause starved by dispatcher holding RLock — B.2 regression")
	}

	// Sanity: job is now paused (or at least Pause observed a consistent
	// state). Read after the goroutine returns.
	if snap := q.SnapshotJob(job.ID); snap != nil && snap.Status != constants.StatusPaused {
		t.Logf("job status after Pause: %v", snap.Status)
	}
}
