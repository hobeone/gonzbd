package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDirectUnpack is a minimal directUnpackWaiter. Wait blocks until finish or
// Abort closes its done channel; Abort records that it was called.
type fakeDirectUnpack struct {
	done    chan struct{}
	once    sync.Once
	aborted atomic.Bool
}

func newFakeDirectUnpack() *fakeDirectUnpack {
	return &fakeDirectUnpack{done: make(chan struct{})}
}

func (f *fakeDirectUnpack) Wait() { <-f.done }

func (f *fakeDirectUnpack) finish() { f.once.Do(func() { close(f.done) }) }

func (f *fakeDirectUnpack) Abort() {
	f.aborted.Store(true)
	f.finish()
}

func TestAwaitDirectUnpackOrAbort_NaturalCompletion(t *testing.T) {
	t.Parallel()
	f := newFakeDirectUnpack()
	f.finish() // unpack already done before we wait

	if !awaitDirectUnpackOrAbort(context.Background(), f) {
		t.Error("expected true (natural completion), got false")
	}
	if f.aborted.Load() {
		t.Error("Abort must not be called when the unpack completes on its own")
	}
}

func TestAwaitDirectUnpackOrAbort_CancelAbortsAndReturns(t *testing.T) {
	t.Parallel()
	f := newFakeDirectUnpack() // Wait() blocks: unpack is stuck (e.g. missing volume)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // lifecycle context cancelled (shutdown)

	// Must return promptly rather than hang: the cancellation path aborts the
	// stuck unpack so Wait() returns. Run in a goroutine bounded by a generous
	// deadline so a regression (missing Abort) fails this test in ~200ms
	// instead of stalling the whole package until the outer go-test timeout.
	// The deadline is a hang-detection window only — the success path returns
	// immediately, so it never adds latency in the normal case.
	result := make(chan bool, 1)
	go func() { result <- awaitDirectUnpackOrAbort(ctx, f) }()
	select {
	case got := <-result:
		if got {
			t.Error("expected false (aborted on cancel), got true")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("awaitDirectUnpackOrAbort did not return promptly after context cancellation")
	}
	if !f.aborted.Load() {
		t.Error("expected Abort to be called on context cancellation")
	}
}
