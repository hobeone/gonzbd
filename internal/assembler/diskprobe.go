package assembler

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"time"
)

// DefaultDiskProbeTTL is the default freshness window for DiskProbe's
// per-directory cache.
const DefaultDiskProbeTTL = 5 * time.Second

// diskProbeEvictAfter is the default idle (no outstanding probe) window a
// probeState entry survives before it becomes eligible for eviction.
// evictStaleLocked never applies a threshold shorter than the instance's
// own ttl, so this default only needs to be reasonable, not an invariant
// callers must maintain by hand — it exists to reclaim entries for
// directories that have stopped being queried altogether (e.g. a completed
// job's directory, which Assembler.checkDiskSpace queries once per unique
// open-file directory and then never again once the job finishes).
const diskProbeEvictAfter = 10 * time.Minute

// DiskProbe bounds repeated FreeBytes queries against a stuck mount to at
// most one outstanding statfs call per directory, with a short TTL cache so
// callers on a healthy mount don't pay a syscall on every request.
//
// FreeBytes and freeBytes already bound any single caller's wait with a
// ctx-select, but each call spawns its own goroutine to wrap the
// uninterruptible statfs syscall — on a permanently stuck NFS/SMB mount,
// every one of those goroutines (and its parked OS thread) is abandoned
// forever, and repeated calls accumulate without bound. DiskProbe fixes
// that by tracking, per directory, whether a probe is already in flight and
// never launching a second one while the first hasn't returned.
//
// DiskProbe.dirs itself is bounded too: a caller like Assembler.checkDiskSpace
// queries one directory per currently-open job, so over a long-running
// daemon's lifetime the set of directories ever queried is effectively
// unbounded (one entry per job ever processed) unless idle entries are
// reclaimed. Idle (no outstanding probe) entries older than the greater of
// diskProbeEvictAfter and the instance's own ttl are swept opportunistically
// whenever a new directory is first queried — see evictStaleLocked.
type DiskProbe struct {
	statfs func(path string, buf *syscall.Statfs_t) error // injectable, defaults to syscall.Statfs
	ttl    time.Duration

	// evictAfter defaults to diskProbeEvictAfter; overridable by tests
	// (same-package seam, set once before first use) so eviction can be
	// exercised without a real 10-minute wait.
	evictAfter time.Duration

	mu   sync.Mutex
	dirs map[string]*probeState
}

// probeState is the per-directory cache/in-flight-tracking state. pending is
// non-nil exactly while a probe goroutine is outstanding for this
// directory; free, err, and fetchedAt are written only by that goroutine
// (under mu) when it completes.
type probeState struct {
	mu        sync.Mutex
	pending   chan struct{}
	free      int64
	err       error
	fetchedAt time.Time
}

// NewDiskProbe returns a DiskProbe that caches statfs results for ttl.
func NewDiskProbe(ttl time.Duration) *DiskProbe {
	return &DiskProbe{
		statfs:     syscall.Statfs,
		ttl:        ttl,
		evictAfter: diskProbeEvictAfter,
		dirs:       make(map[string]*probeState),
	}
}

// FreeBytes returns the number of bytes available to unprivileged processes
// on the filesystem containing dir, per the same semantics as the
// package-level FreeBytes, but deduped and cached: concurrent or rapid
// repeated calls for the same directory share at most one in-flight statfs
// call, and a completed result is reused for ttl before a fresh probe is
// launched.
//
// ctx must be non-nil and cancellable (ctx.Done() != nil), matching
// FreeBytes' invariant — a nil-Done() ctx would make the caller's select
// below degenerate into an unbounded wait.
func (p *DiskProbe) FreeBytes(ctx context.Context, dir string) (int64, error) {
	if ctx == nil || ctx.Done() == nil {
		return 0, fmt.Errorf("assembler: DiskProbe.FreeBytes requires a cancellable context; " +
			"statfs can block forever on an unreachable mount")
	}

	pending, free, err, hit := p.claim(dir)
	if hit {
		return free, err
	}

	select {
	case <-pending.done:
		pending.st.mu.Lock()
		free, err := pending.st.free, pending.st.err
		pending.st.mu.Unlock()
		return free, err
	case <-ctx.Done():
		return 0, fmt.Errorf("assembler: statfs %s: %w", dir, ctx.Err())
	}
}

// claimResult carries the probeState alongside its pending channel so
// FreeBytes' post-select read doesn't need a second map lookup (which could
// itself race against eviction).
type claimResult struct {
	st   *probeState
	done chan struct{}
}

// claim returns dir's cached result if fresh (hit == true), or otherwise
// ensures a probe is in flight and returns the state to wait on.
//
// The get-or-create, eviction sweep, freshness check, and pending-claim
// all run under one p.mu critical section, by design: eviction and
// claiming a directory's entry must never interleave, since an entry
// evicted between being handed to a caller and that caller marking it
// pending would let a later caller create a fresh entry and launch a
// second, duplicate probe for the same directory — the exact violation of
// "at most one outstanding probe per directory" this type exists to
// prevent.
func (p *DiskProbe) claim(dir string) (result claimResult, free int64, err error, hit bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.dirs[dir]
	if !ok {
		p.evictStaleLocked()
		st = &probeState{}
		p.dirs[dir] = st
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pending == nil && !st.fetchedAt.IsZero() && time.Since(st.fetchedAt) < p.ttl {
		return claimResult{}, st.free, st.err, true
	}
	if st.pending == nil {
		pending := make(chan struct{})
		st.pending = pending
		go p.probe(st, dir, pending)
	}
	return claimResult{st: st, done: st.pending}, 0, nil, false
}

// evictStaleLocked removes probeState entries that have been idle (no
// outstanding probe, and no fresh-enough cached result to be worth keeping
// around) for longer than diskProbeEvictAfter. Called with p.mu held.
//
// An entry with a still-outstanding probe (pending != nil) is never
// evicted, regardless of how long ago it was created — deleting it while a
// detached probe goroutine is still running would let the next caller for
// that directory spawn a second, duplicate probe, silently reintroducing
// the per-directory bound this type exists to enforce (this matters most
// on exactly the permanently-stuck-mount case, where pending never clears
// and fetchedAt may still be its zero value).
func (p *DiskProbe) evictStaleLocked() {
	// The eviction threshold must never be shorter than the cache's own
	// TTL — an idle entry within its TTL window is still a valid cache
	// hit, and evicting it early would silently force a fresh probe on
	// the next call despite the cached value still being fresh by the
	// caller's own contract. evictAfter defaults far above any reasonable
	// TTL, but a future NewDiskProbe(ttl) call with ttl >= evictAfter
	// would otherwise defeat this silently.
	threshold := max(p.evictAfter, p.ttl)
	for dir, st := range p.dirs {
		st.mu.Lock()
		stale := st.pending == nil && !st.fetchedAt.IsZero() && time.Since(st.fetchedAt) > threshold
		st.mu.Unlock()
		if stale {
			delete(p.dirs, dir)
		}
	}
}

// probe runs the actual statfs call detached from any single caller's ctx —
// it must outlive whichever caller triggered it so it can finish and
// populate the cache for later callers even if this one gave up waiting.
// It is the sole writer of st.free/st.err/st.fetchedAt/st.pending.
func (p *DiskProbe) probe(st *probeState, dir string, pending chan struct{}) {
	free, err := statfsFreeBytes(dir, p.statfs)
	st.mu.Lock()
	st.free, st.err, st.fetchedAt = free, err, time.Now()
	st.pending = nil
	st.mu.Unlock()
	close(pending)
}
