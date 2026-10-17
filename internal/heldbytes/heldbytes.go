// Package heldbytes bounds, in bytes, the decoded article data held between
// the decoder and the disk.
//
// The downloader acquires an article's share before it fetches the article
// and attaches the Hold to the decoded result; whoever ends the decoded
// buffer's life releases it. The queues between decode and write count
// articles, not bytes, so on their own they do not bound memory.
package heldbytes

import (
	"context"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

// Budget is a byte budget that Acquire blocks on once it is spent.
//
// A semaphore rather than a mutex and counter: Acquire must block without
// holding a lock (scripts/check_lock_io), wake on a release or on its
// context, and serve waiters in order so a large article is not starved by a
// stream of small ones. golang.org/x/sync/semaphore.Weighted does all three.
type Budget struct {
	sem   *semaphore.Weighted
	limit int64

	// held mirrors the semaphore's count, which Weighted does not expose.
	// Changed by Acquire and Hold.Release only.
	held atomic.Int64
}

// New returns a Budget of limit bytes. limit must be positive.
func New(limit int64) *Budget {
	if limit <= 0 {
		panic("heldbytes: limit must be positive")
	}
	return &Budget{sem: semaphore.NewWeighted(limit), limit: limit}
}

// Acquire blocks until n bytes fit in the budget, or ctx ends, in which case
// it returns ctx's error and holds nothing. An n above the whole budget is
// charged as the whole budget, so it proceeds once nothing else is held
// rather than waiting forever.
func (b *Budget) Acquire(ctx context.Context, n int64) (*Hold, error) {
	n = min(max(n, 0), b.limit)
	if err := b.sem.Acquire(ctx, n); err != nil {
		return nil, err
	}
	b.held.Add(n)
	return &Hold{budget: b, n: n}, nil
}

// Held is the number of bytes currently acquired and not released.
func (b *Budget) Held() int64 { return b.held.Load() }

// Hold is one acquirer's share of a Budget, from Acquire until Release. A nil
// *Hold holds nothing and releases nothing, which is what a result carrying no
// decoded data has.
type Hold struct {
	budget   *Budget
	n        int64
	released atomic.Bool
}

// Release gives the bytes back. Only the first call releases; a later one is
// a no-op, so two paths that each end an article's life cannot give its bytes
// back twice.
func (h *Hold) Release() {
	if h == nil || !h.released.CompareAndSwap(false, true) {
		return
	}
	h.budget.held.Add(-h.n)
	h.budget.sem.Release(h.n)
}
