package downloader

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestDispatchPass_ExhaustedEmitsDoNotBlockQueueWriters is the regression
// guard for the B.2 deadlock.
//
// Before the fix, tryDispatch emitted ErrNoServersLeft inline while holding
// both tryMu and the queue RLock taken by Queue.ForEachUnfinishedArticle. If
// the completions channel was full, the dispatcher blocked forever — and so
// did any goroutine trying to take the queue write lock (e.g. the pipeline
// consumer wanting to mark an article failed), because the RLock was still
// held.
//
// forEachUnfinishedArticle (dispatch.go) no longer holds one lock across the
// whole walk -- job.Job.Manifest()/Progress() each take and release
// contentMu per call -- so the specific lock this test names no longer
// exists to hold. The test is kept because the general property (blocking on
// a full completions channel must not starve an unrelated job mutation) is
// still worth pinning, now exercised through JobSource.PauseJob rather than
// Queue.Pause.
//
// The test pins the buffer at 1, lets exhausted emits fill it with no
// consumer draining, then asserts a job-mutating call (PauseJob) still
// completes promptly.
func TestDispatchPass_ExhaustedEmitsDoNotBlockQueueWriters(t *testing.T) {
	ms := newMockNNTP(t)
	// No articles added — every BODY request gets 430.

	q := newFakeJobSource()
	tj := makeJobWithArticles(t, []string{"a@h", "b@h", "c@h"})
	q.add(tj)

	srv := testServer(t, "only", ms.addr)
	d := New(q, []*Server{srv}, nil, Options{CompletionsBuffer: 1}, nil)
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
	// on the next exhausted emit while holding the queue RLock. The assertion
	// below is guarded by its own 2s timeout, so an over/under-sized window
	// cannot cause a false failure — only a narrower reproduction window.
	time.Sleep(200 * time.Millisecond)

	// A job mutation must make progress even while the dispatcher is
	// blocked on a full completions channel. Pre-B.2 this call deadlocked
	// because the dispatcher held the queue RLock.
	done := make(chan error, 1)
	go func() { done <- q.PauseJob(tj.ID()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("PauseJob returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("PauseJob starved by the dispatcher — B.2 regression")
	}

	// Sanity: job is now paused. Read after the goroutine returns.
	if tj.Intent() != job.IntentPause {
		t.Logf("job intent after PauseJob: %v", tj.Intent())
	}
}
