package assembler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestDiskProbe_RejectsInvalidContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context //nolint:containedctx // table-driven test input, not stored state
	}{
		{"nil", nil},
		{"background", context.Background()}, //nolint:usetesting // deliberately testing context.Background(), not t.Context()
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewDiskProbe(DefaultDiskProbeTTL)
			_, err := p.FreeBytes(tt.ctx, t.TempDir())
			if err == nil {
				t.Fatalf("expected error for %s context, got nil", tt.name)
			}
		})
	}
}

func TestDiskProbe_CachesWithinTTL(t *testing.T) {
	var calls atomic.Int32
	p := NewDiskProbe(time.Hour) // long TTL so the test can't flake on timing
	p.statfs = func(path string, buf *syscall.Statfs_t) error {
		calls.Add(1)
		return syscall.Statfs(path, buf)
	}

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := p.FreeBytes(ctx, dir); err != nil {
		t.Fatalf("first FreeBytes: %v", err)
	}
	if _, err := p.FreeBytes(ctx, dir); err != nil {
		t.Fatalf("second FreeBytes: %v", err)
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("statfs called %d times, want 1 (second call should hit cache)", got)
	}
}

func TestDiskProbe_RefreshesAfterTTLExpiryOnceIdle(t *testing.T) {
	var calls atomic.Int32
	const ttl = 20 * time.Millisecond
	p := NewDiskProbe(ttl)
	p.statfs = func(path string, buf *syscall.Statfs_t) error {
		calls.Add(1)
		return syscall.Statfs(path, buf)
	}

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := p.FreeBytes(ctx, dir); err != nil {
		t.Fatalf("first FreeBytes: %v", err)
	}
	time.Sleep(ttl * 3)
	if _, err := p.FreeBytes(ctx, dir); err != nil {
		t.Fatalf("second FreeBytes: %v", err)
	}

	if got := calls.Load(); got != 2 {
		t.Errorf("statfs called %d times, want 2 (cache should have expired)", got)
	}
}

// TestDiskProbe_ClaimAndEvictionNeverRaceToDuplicateProbe stress-tests the
// invariant that eviction and claiming a directory's probeState can never
// interleave to produce two concurrent probes for the same directory. The
// scenario: a "hot" directory has an idle, stale entry; a concurrent claim
// for a brand-new directory triggers evictStaleLocked (eviction only runs
// as a side effect of inserting a genuinely new map key — see claim's
// !ok branch), which could delete the hot directory's entry between the
// moment a caller for it is handed that entry and the moment it marks it
// pending — if get-or-create, eviction, and pending-claim aren't one atomic
// p.mu-held decision, a second caller for the hot directory can then create
// a fresh entry and launch a duplicate probe.
//
// A steady stream of goroutines requesting brand-new "cold" directory
// names (via an atomic counter, guaranteeing every one is a genuinely new
// key) keeps eviction firing continuously throughout the run — reusing a
// fixed small set for all traffic would let every entry get created once
// during warm-up and then never trigger eviction's new-key side effect
// again, which is a dead test. Fabricated (non-existent) paths are used
// throughout — the injected fake statfs never touches the real filesystem,
// so no real directories are needed.
func TestDiskProbe_ClaimAndEvictionNeverRaceToDuplicateProbe(t *testing.T) {
	const numHotDirs = 3
	const numGoroutines = 40
	const stressDuration = 300 * time.Millisecond

	hotDirs := make([]string, numHotDirs)
	for i := range hotDirs {
		hotDirs[i] = fmt.Sprintf("/nonexistent/hot-%d", i)
	}
	var coldCounter atomic.Int64

	p := NewDiskProbe(2 * time.Millisecond)
	p.evictAfter = 2 * time.Millisecond

	var mu sync.Mutex
	inFlight := make(map[string]int)
	var violated atomic.Bool
	p.statfs = func(path string, buf *syscall.Statfs_t) error {
		mu.Lock()
		inFlight[path]++
		n := inFlight[path]
		mu.Unlock()
		if n > 1 {
			violated.Store(true)
		}
		time.Sleep(200 * time.Microsecond) // widen the window so an overlap is observable
		mu.Lock()
		inFlight[path]--
		mu.Unlock()
		return syscall.Statfs(path, buf) //nolint:errcheck // fabricated path; error return is expected and irrelevant here
	}

	deadline := time.Now().Add(stressDuration)
	var wg sync.WaitGroup
	for g := range numGoroutines {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			i := 0
			for time.Now().Before(deadline) {
				var dir string
				if i%2 == 0 {
					dir = hotDirs[(seed+i)%numHotDirs]
				} else {
					dir = fmt.Sprintf("/nonexistent/cold-%d", coldCounter.Add(1))
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond) //nolint:usetesting // detached from t.Context() so calls outlive individual loop iterations cleanly
				_, _ = p.FreeBytes(ctx, dir)
				cancel()
				i++
			}
		}(g)
	}
	wg.Wait()

	if violated.Load() {
		t.Fatal("more than one statfs probe was in flight for the same directory at once — " +
			"claim/eviction race let a duplicate probe start")
	}
}

// TestDiskProbe_EvictsIdleEntriesButNotPendingOnes proves DiskProbe.dirs
// does not grow without bound over the life of a long-running daemon that
// queries many distinct, short-lived directories (e.g. one per completed
// download job) — while also proving eviction never touches a directory
// whose probe is still outstanding, which would otherwise let a later
// caller spawn a second, duplicate probe for that directory.
func TestDiskProbe_EvictsIdleEntriesButNotPendingOnes(t *testing.T) {
	// ttl must not exceed evictAfter here — eviction never applies a
	// threshold shorter than ttl (see TestDiskProbe_EvictionNeverShorterThanTTL),
	// so both need to be this test's short duration for the sweep to fire.
	const shortDuration = 10 * time.Millisecond
	p := NewDiskProbe(shortDuration)
	p.evictAfter = shortDuration
	p.statfs = func(path string, buf *syscall.Statfs_t) error {
		return syscall.Statfs(path, buf)
	}

	idleDir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := p.FreeBytes(ctx, idleDir); err != nil {
		t.Fatalf("FreeBytes(idleDir): %v", err)
	}

	// Wedge a second directory's probe forever, so its entry must survive
	// eviction despite never completing (fetchedAt stays zero-valued).
	stuckDir := t.TempDir()
	unblock := make(chan struct{})
	defer close(unblock) // let the probe goroutine finish before the test exits
	p.statfs = func(path string, buf *syscall.Statfs_t) error {
		if path == stuckDir {
			<-unblock
		}
		return syscall.Statfs(path, buf)
	}
	stuckCtx, stuckCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer stuckCancel()
	if _, err := p.FreeBytes(stuckCtx, stuckDir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FreeBytes(stuckDir) error = %v, want context.DeadlineExceeded", err)
	}

	time.Sleep(p.evictAfter * 3) // let idleDir's entry age past evictAfter

	// Triggering a lookup for a brand-new directory is what runs the
	// opportunistic sweep (claim only sweeps when inserting a new key).
	thirdDir := t.TempDir()
	if _, err := p.FreeBytes(ctx, thirdDir); err != nil {
		t.Fatalf("FreeBytes(thirdDir): %v", err)
	}

	p.mu.Lock()
	_, idleStillPresent := p.dirs[idleDir]
	_, stuckStillPresent := p.dirs[stuckDir]
	_, thirdPresent := p.dirs[thirdDir]
	p.mu.Unlock()

	if idleStillPresent {
		t.Error("idle, completed entry was not evicted after aging past evictAfter")
	}
	if !stuckStillPresent {
		t.Fatal("entry with a still-outstanding probe was evicted — a later caller would spawn a duplicate probe for the same directory")
	}
	if !thirdPresent {
		t.Error("the directory that triggered the sweep should itself remain in the map")
	}
}

// TestDiskProbe_EvictionNeverShorterThanTTL proves that a TTL configured
// longer than evictAfter cannot be silently defeated by eviction: an idle
// entry within its own TTL window must still serve as a cache hit (no new
// statfs call), even after evictAfter alone would have expired it.
func TestDiskProbe_EvictionNeverShorterThanTTL(t *testing.T) {
	var calls atomic.Int32
	p := NewDiskProbe(50 * time.Millisecond) // ttl deliberately longer than evictAfter below
	p.evictAfter = 10 * time.Millisecond
	p.statfs = func(path string, buf *syscall.Statfs_t) error {
		calls.Add(1)
		return syscall.Statfs(path, buf)
	}

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := p.FreeBytes(ctx, dir); err != nil {
		t.Fatalf("first FreeBytes: %v", err)
	}

	// Past evictAfter but still within ttl.
	time.Sleep(p.evictAfter * 3)

	// A lookup for a different directory is what triggers the opportunistic
	// sweep; dir's entry must survive it because it's still within ttl.
	otherDir := t.TempDir()
	if _, err := p.FreeBytes(ctx, otherDir); err != nil {
		t.Fatalf("FreeBytes(otherDir): %v", err)
	}

	if _, err := p.FreeBytes(ctx, dir); err != nil {
		t.Fatalf("second FreeBytes(dir): %v", err)
	}

	if got := calls.Load(); got != 2 {
		t.Errorf("statfs called %d times, want 2 (dir's still-fresh entry should have been reused, not re-probed)", got)
	}
}

// TestDiskProbe_BoundsConcurrentCallersToOneProbe is the core regression
// test for the bug this type fixes: on a mount that never responds, a
// naive design (spawn-a-goroutine-per-call, even with a bare TTL cache)
// leaks one goroutine per call forever. DiskProbe must dedupe all of them
// onto a single in-flight probe.
func TestDiskProbe_BoundsConcurrentCallersToOneProbe(t *testing.T) {
	baseline := goleak.IgnoreCurrent()

	var calls atomic.Int32
	unblock := make(chan struct{})
	p := NewDiskProbe(DefaultDiskProbeTTL)
	p.statfs = func(path string, buf *syscall.Statfs_t) error {
		calls.Add(1)
		<-unblock
		return syscall.Statfs(path, buf)
	}

	dir := t.TempDir()

	const callers = 8
	done := make(chan struct{}, callers)
	for range callers {
		go func() {
			defer func() { done <- struct{}{} }()
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			_, err := p.FreeBytes(ctx, dir)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("FreeBytes error = %v, want context.DeadlineExceeded", err)
			}
		}()
	}
	for range callers {
		<-done
	}

	// A second wave of callers arriving after the first wave's ctx expired,
	// while the probe is still blocked — must not spawn a second probe.
	for range callers {
		go func() {
			defer func() { done <- struct{}{} }()
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			_, err := p.FreeBytes(ctx, dir)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("FreeBytes error = %v, want context.DeadlineExceeded", err)
			}
		}()
	}
	for range callers {
		<-done
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("statfs invoked %d times across %d callers in two waves, want 1", got, 2*callers)
	}

	close(unblock)
	assertNoLeakedGoroutines(t, baseline)
}

// TestDiskProbe_LateCallerSeesFreshResultAfterProbeCompletes proves the
// cache is actually populated by a probe that outlives the caller who
// triggered it, not just that later callers avoid spawning a new one.
func TestDiskProbe_LateCallerSeesFreshResultAfterProbeCompletes(t *testing.T) {
	baseline := goleak.IgnoreCurrent()

	unblock := make(chan struct{})
	p := NewDiskProbe(DefaultDiskProbeTTL)
	p.statfs = func(path string, buf *syscall.Statfs_t) error {
		<-unblock
		return syscall.Statfs(path, buf)
	}

	dir := t.TempDir()

	ctx1, cancel1 := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel1()
	if _, err := p.FreeBytes(ctx1, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first FreeBytes error = %v, want context.DeadlineExceeded", err)
	}

	close(unblock)

	// Give the detached probe goroutine a moment to finish and populate the
	// cache, then confirm a later caller gets a real value with no error.
	deadline := time.Now().Add(2 * time.Second)
	for {
		ctx2, cancel2 := context.WithTimeout(t.Context(), 500*time.Millisecond)
		free, err := p.FreeBytes(ctx2, dir)
		cancel2()
		if err == nil {
			if free <= 0 {
				t.Fatalf("FreeBytes() = %d, want > 0 for a real temp dir", free)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("late caller never observed a completed probe result: %v", err)
		}
	}

	assertNoLeakedGoroutines(t, baseline)
}
