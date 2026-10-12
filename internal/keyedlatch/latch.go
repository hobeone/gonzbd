// Package keyedlatch provides Latch: at most one claim per string key, which
// a waiter can wait on with a context.
//
// A claim is a channel rather than a mutex per key because a waiter has to be
// able to give up when its context ends, and sync.Mutex.Lock cannot.
package keyedlatch

import (
	"context"
	"sync"
)

// Latch holds at most one claim per key. Its zero value is ready to use, and
// it must not be copied after first use.
//
// mu is a leaf: it is held only across a map operation and a close, so a
// caller may hold its own lock while calling TryClaim, Release, Held or Done,
// and use that lock to make a claim atomic with state of its own. Wait blocks,
// so a caller must not hold a lock the claim's releaser needs while calling
// it.
type Latch struct {
	mu   sync.Mutex
	held map[string]chan struct{} // a key is present only while claimed; its channel closes on release
}

// TryClaim claims key if no one holds it, and reports whether it did. It
// never waits.
func (l *Latch) TryClaim(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, busy := l.held[key]; busy {
		return false
	}
	if l.held == nil {
		l.held = make(map[string]chan struct{})
	}
	l.held[key] = make(chan struct{})
	return true
}

// Release frees key and wakes every waiter on the claim it ends. Releasing a
// key no one holds does nothing.
func (l *Latch) Release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch, ok := l.held[key]; ok {
		close(ch)
		delete(l.held, key)
	}
}

// Held reports whether someone holds key.
func (l *Latch) Held(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.held[key]
	return ok
}

// Done returns a channel that is closed when the claim on key held now is
// released, or nil if no one holds key. A later claim on key gets a channel of
// its own, so the one returned does not follow it.
func (l *Latch) Done(key string) <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch, ok := l.held[key]; ok {
		return ch
	}
	return nil
}

// Wait waits until the claim on key held now is released, and returns nil. It
// returns nil at once if no one holds key, and ctx.Err() if ctx ends first.
// When both have happened, the release wins. It waits for that one claim: a
// claim taken after the release does not extend the wait, so a caller that
// wants the key goes round TryClaim again.
func (l *Latch) Wait(ctx context.Context, key string) error {
	ch := l.Done(key)
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	default:
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
