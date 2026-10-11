package keyedlatch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitResult runs l.Wait(ctx, key) on its own goroutine and returns its
// result, failing the test if it has not returned within d.
func waitResult(t *testing.T, l *Latch, ctx context.Context, key string, d time.Duration) error {
	t.Helper()
	got := make(chan error, 1)
	go func() { got <- l.Wait(ctx, key) }()
	select {
	case err := <-got:
		return err
	case <-time.After(d):
		t.Fatalf("Wait(%q) did not return within %v", key, d)
		return nil
	}
}

func TestLatch_TryClaimRefusesAHeldKeyAndTakesItAfterRelease(t *testing.T) {
	t.Parallel()
	var l Latch
	if !l.TryClaim("a") {
		t.Fatal("TryClaim of a free key = false, want true")
	}
	if !l.Held("a") {
		t.Error("Held after TryClaim = false, want true")
	}
	if l.TryClaim("a") {
		t.Error("TryClaim of a held key = true, want false")
	}
	if !l.TryClaim("b") {
		t.Error("TryClaim of another free key = false, want true: keys are independent")
	}
	l.Release("a")
	if l.Held("a") {
		t.Error("Held after Release = true, want false")
	}
	if !l.TryClaim("a") {
		t.Error("TryClaim after Release = false, want true")
	}
}

func TestLatch_ReleaseOfAnUnheldKeyDoesNothing(t *testing.T) {
	t.Parallel()
	var l Latch
	l.Release("a") // zero value, nothing held
	if !l.TryClaim("b") {
		t.Fatal("TryClaim(b) = false")
	}
	l.Release("a")
	if !l.Held("b") {
		t.Error("Release of an unheld key released another")
	}
	l.Release("b")
	l.Release("b") // a second release of the same key is a no-op too
	if l.Held("b") {
		t.Error("b still held after Release")
	}
}

// Release drops the key from the map, so the map does not grow with every key
// ever claimed.
func TestLatch_ReleaseRemovesTheKey(t *testing.T) {
	t.Parallel()
	var l Latch
	for _, k := range []string{"a", "b", "c"} {
		l.TryClaim(k)
		l.Release(k)
	}
	l.mu.Lock()
	n := len(l.held)
	l.mu.Unlock()
	if n != 0 {
		t.Errorf("%d keys left in the map after release, want 0", n)
	}
}

func TestLatch_WaitOnAnUnheldKeyReturnsAtOnce(t *testing.T) {
	t.Parallel()
	var l Latch
	if err := waitResult(t, &l, context.Background(), "a", 5*time.Second); err != nil {
		t.Errorf("Wait on an unheld key = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitResult(t, &l, ctx, "a", 5*time.Second); err != nil {
		t.Errorf("Wait on an unheld key with an ended context = %v, want nil", err)
	}
}

// Release wakes every waiter on the claim, not one of them.
func TestLatch_ReleaseWakesEveryWaiter(t *testing.T) {
	t.Parallel()
	var l Latch
	l.TryClaim("a")
	const waiters = 8
	done := make(chan error, waiters)
	for range waiters {
		go func() { done <- l.Wait(context.Background(), "a") }()
	}
	select {
	case err := <-done:
		t.Fatalf("a waiter returned (%v) while the key was held", err)
	case <-time.After(50 * time.Millisecond):
	}
	l.Release("a")
	deadline := time.After(5 * time.Second)
	for range waiters {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Wait = %v after release, want nil", err)
			}
		case <-deadline:
			t.Fatal("not every waiter woke on the release")
		}
	}
}

// A waiter gives up when its context ends while the key is still held.
func TestLatch_WaitReturnsTheContextErrorWhenItEnds(t *testing.T) {
	t.Parallel()
	var l Latch
	l.TryClaim("a")

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { got <- l.Wait(ctx, "a") }()
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Wait after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return when its context was cancelled")
	}

	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer dcancel()
	if err := waitResult(t, &l, dctx, "a", 5*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait past its deadline = %v, want context.DeadlineExceeded", err)
	}
	if !l.Held("a") {
		t.Error("a waiter giving up released the claim")
	}
}

// A claim already released wins over an ended context: Wait looks at the
// claim before it looks at ctx, rather than letting select pick at random.
// The closed channel stands in for a claim released between Wait's lookup and
// its select, a window no seam reaches.
func TestLatch_WaitPrefersAReleaseOverAnEndedContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 200 {
		var l Latch
		closed := make(chan struct{})
		close(closed)
		l.held = map[string]chan struct{}{"a": closed}
		if err := l.Wait(ctx, "a"); err != nil {
			t.Fatalf("iteration %d: Wait = %v, want nil: a released claim must win over an ended context", i, err)
		}
	}
}

// Done follows the claim held when it is called: a later claim on the same key
// has a channel of its own, so a waiter on the first is not held by the second.
func TestLatch_DoneFollowsOneClaim(t *testing.T) {
	t.Parallel()
	var l Latch
	if l.Done("a") != nil {
		t.Fatal("Done of an unheld key is not nil")
	}
	l.TryClaim("a")
	first := l.Done("a")
	if first == nil {
		t.Fatal("Done of a held key is nil")
	}
	select {
	case <-first:
		t.Fatal("Done's channel closed while the claim is held")
	default:
	}
	l.Release("a")
	l.TryClaim("a")
	select {
	case <-first:
	default:
		t.Fatal("the first claim's channel did not close on its release")
	}
	if second := l.Done("a"); second == nil || second == first {
		t.Error("the second claim did not get a channel of its own")
	}
}

// Many goroutines claim, wait and release a few keys: at most one holds each
// key at a time, and every one gets through. Run under -race.
func TestLatch_ConcurrentClaimsExcludeEachOther(t *testing.T) {
	t.Parallel()
	var l Latch
	keys := []string{"a", "b", "c"}
	var holders [3]atomic.Int32
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			k := g % len(keys)
			for range 200 {
				for !l.TryClaim(keys[k]) {
					if err := l.Wait(context.Background(), keys[k]); err != nil {
						t.Errorf("Wait: %v", err)
						return
					}
				}
				if n := holders[k].Add(1); n != 1 {
					t.Errorf("%d holders of %q at once", n, keys[k])
				}
				holders[k].Add(-1)
				l.Release(keys[k])
			}
		})
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("claimers did not finish: a release did not wake a waiter")
	}
	for _, k := range keys {
		if l.Held(k) {
			t.Errorf("%q still held after every claimer released", k)
		}
	}
}
