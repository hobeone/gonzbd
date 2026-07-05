package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
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
	// stuck unpack so Wait() returns. If the abort were missing, this call
	// would block forever and the test would time out.
	if awaitDirectUnpackOrAbort(ctx, f) {
		t.Error("expected false (aborted on cancel), got true")
	}
	if !f.aborted.Load() {
		t.Error("expected Abort to be called on context cancellation")
	}
}
