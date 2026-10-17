package heldbytes

import (
	"context"
	"errors"
	"testing"
	"time"
)

// blockedWindow is how long a test watches an acquirer it expects to stay
// blocked. It is a negative observation, not synchronisation: a wrongly
// unblocked acquirer returns within microseconds, far inside it.
const blockedWindow = 50 * time.Millisecond

// acquireAsync runs Acquire on its own goroutine and delivers the outcome.
func acquireAsync(ctx context.Context, b *Budget, n int64) <-chan acquireResult {
	ch := make(chan acquireResult, 1)
	go func() {
		h, err := b.Acquire(ctx, n)
		ch <- acquireResult{h, err}
	}()
	return ch
}

type acquireResult struct {
	hold *Hold
	err  error
}

func requireBlocked(t *testing.T, ch <-chan acquireResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("Acquire returned (%v, %v) while the budget was full", r.hold, r.err)
	case <-time.After(blockedWindow):
	}
}

func requireAcquired(t *testing.T, ch <-chan acquireResult) *Hold {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("Acquire: %v", r.err)
		}
		return r.hold
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire still blocked after 5s")
		return nil
	}
}

func TestBudget_AccountingReturnsToZero(t *testing.T) {
	b := New(100)
	sizes := []int64{10, 20, 30, 40}
	holds := make([]*Hold, 0, len(sizes))
	for _, n := range sizes {
		h, err := b.Acquire(t.Context(), n)
		if err != nil {
			t.Fatalf("Acquire(%d): %v", n, err)
		}
		holds = append(holds, h)
	}
	if got := b.Held(); got != 100 {
		t.Fatalf("Held() = %d after acquiring 100, want 100", got)
	}
	for _, h := range holds {
		h.Release()
	}
	if got := b.Held(); got != 0 {
		t.Fatalf("Held() = %d after releasing everything, want 0", got)
	}
}

func TestBudget_BlockedAcquirerUnblocksOnRelease(t *testing.T) {
	b := New(100)
	first, err := b.Acquire(t.Context(), 80)
	if err != nil {
		t.Fatal(err)
	}
	waiting := acquireAsync(t.Context(), b, 30)
	requireBlocked(t, waiting)

	first.Release()
	second := requireAcquired(t, waiting)
	if got := b.Held(); got != 30 {
		t.Fatalf("Held() = %d, want 30", got)
	}
	second.Release()
}

func TestBudget_BlockedAcquirerUnblocksOnContextCancel(t *testing.T) {
	b := New(100)
	first, err := b.Acquire(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	ctx, cancel := context.WithCancel(t.Context())
	waiting := acquireAsync(ctx, b, 1)
	requireBlocked(t, waiting)
	cancel()

	select {
	case r := <-waiting:
		if !errors.Is(r.err, context.Canceled) || r.hold != nil {
			t.Fatalf("Acquire after cancel = (%v, %v), want (nil, context.Canceled)", r.hold, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire still blocked 5s after its context was cancelled")
	}
	if got := b.Held(); got != 100 {
		t.Fatalf("Held() = %d after a cancelled Acquire, want 100", got)
	}
}

func TestBudget_OversizedAcquireProceedsWhenNothingIsHeld(t *testing.T) {
	b := New(100)

	// Alone, it is admitted at once and charged the whole budget.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	big, err := b.Acquire(ctx, 1000)
	if err != nil {
		t.Fatalf("oversized Acquire with nothing held: %v", err)
	}
	if got := b.Held(); got != 100 {
		t.Fatalf("Held() = %d for an oversized hold, want the whole budget 100", got)
	}
	big.Release()

	// Behind another hold, it waits for that one, then proceeds.
	small, err := b.Acquire(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	waiting := acquireAsync(t.Context(), b, 1000)
	requireBlocked(t, waiting)
	small.Release()
	requireAcquired(t, waiting).Release()
	if got := b.Held(); got != 0 {
		t.Fatalf("Held() = %d, want 0", got)
	}
}

func TestHold_ReleaseIsIdempotent(t *testing.T) {
	b := New(100)
	h, err := b.Acquire(t.Context(), 40)
	if err != nil {
		t.Fatal(err)
	}
	other, err := b.Acquire(t.Context(), 60)
	if err != nil {
		t.Fatal(err)
	}
	h.Release()
	h.Release()
	if got := b.Held(); got != 60 {
		t.Fatalf("Held() = %d after releasing one hold twice, want 60", got)
	}
	other.Release()

	var none *Hold
	none.Release() // a nil hold releases nothing and does not panic
}

func TestNew_RefusesANonPositiveLimit(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New(%d) did not panic", limit)
				}
			}()
			New(limit)
		}()
	}
}
