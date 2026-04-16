package bpsmeter

import (
	"context"
	"math"
	"sync"

	"golang.org/x/time/rate"
)

// minBurst is the minimum token-bucket burst size when limiting is active.
// A burst of at least ~256 KiB ensures large reads are not serialised into
// thousands of tiny sleeps even at moderate rates. The x/time/rate package
// requires burst >= 1 whenever limit != Inf.
const minBurst = 256 * 1024 // 256 KiB

// Limiter wraps rate.Limiter with a live-updatable rate.
// Zero or negative rate ⇒ unlimited (every Wait returns immediately).
//
// Design choice: when disabled we store a nil *rate.Limiter and skip the call
// to WaitN entirely, avoiding any allocation. This is slightly cleaner than
// using rate.Inf because it makes the "unlimited" path a simple nil-check with
// no token-accounting overhead.
type Limiter struct {
	mu  sync.RWMutex
	lim *rate.Limiter // nil when unlimited
}

// NewLimiter creates a Limiter. bytesPerSec <= 0 means unlimited.
func NewLimiter(bytesPerSec float64) *Limiter {
	l := &Limiter{}
	l.SetRate(bytesPerSec)
	return l
}

// SetRate changes the allowed bytes/sec. A value <= 0 disables limiting.
// Active WaitN callers are not cancelled; they complete against the new
// effective rate when the next token becomes available.
//
// Burst is set to max(bytesPerSec, minBurst) so that a single large read
// (e.g. a full NZB article) does not stall unnecessarily when the rate is
// generous but tokens are consumed in large chunks.
func (l *Limiter) SetRate(bytesPerSec float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if bytesPerSec <= 0 {
		l.lim = nil
		return
	}

	burst := int(math.Ceil(bytesPerSec))
	if burst < minBurst {
		burst = minBurst
	}

	if l.lim == nil {
		l.lim = rate.NewLimiter(rate.Limit(bytesPerSec), burst)
	} else {
		// SetLimit and SetBurst are both safe under contention in x/time/rate.
		// Update limit first: if the new rate is higher, existing waiters may
		// unblock sooner. If lower, burst caps the immediate token grant.
		l.lim.SetLimit(rate.Limit(bytesPerSec))
		l.lim.SetBurst(burst)
	}
}

// Wait blocks until n bytes worth of tokens are available or ctx is done.
// When the limiter is disabled (rate <= 0), returns immediately.
func (l *Limiter) Wait(ctx context.Context, n int) error {
	l.mu.RLock()
	lim := l.lim
	l.mu.RUnlock()

	if lim == nil {
		return nil
	}
	return lim.WaitN(ctx, n) //nolint:wrapcheck // pass through context error unchanged
}
