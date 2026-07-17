package app

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

// raceLoadDuration is how long runReloadUnderLoad drives its readers against a
// concurrent writer. This is a genuine timing window, not synchronisation: the
// race detector can only report a write/read pair it actually observes
// interleaved, so the readers must be given real wall-clock time to overlap a
// reload. It is deliberately short; -count=100 supplies the repetition.
const raceLoadDuration = 200 * time.Millisecond

// runReloadUnderLoad drives Application's unsynchronised readers concurrently
// with ReloadDownloader, which swaps app.downloader and app.downloaderStats
// under app.mu.
//
// It exists to give the race detector an interleaving to observe. Both fields
// are interface values (two words: type pointer + data pointer), so a torn read
// is not a stale number — it can pair the type word of the old downloader with
// the data word of the new one and dispatch into hyperspace. See issue #98.
//
// The reader set intentionally mixes a locked reader (PauseDownloads) with the
// unlocked ones so a regression that removes locking from the correct readers
// is caught too.
func runReloadUnderLoad(t *testing.T, app *Application, d time.Duration) {
	t.Helper()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	spin := func(fn func()) {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					fn()
				}
			}
		})
	}

	// Unlocked readers of app.downloaderStats / app.downloader (the bug).
	spin(func() { _ = app.Speed() })
	spin(func() { _ = app.ServerStatus() })
	// A correctly-locked reader, to prove the harness does not report a false
	// positive against code that already takes app.mu.
	spin(func() { app.PauseDownloads() })

	// The writer: swaps both interface fields under app.mu.
	spin(func() {
		// An empty server list keeps this cheap: downloader.New builds no
		// connection workers, so no sockets are dialled, but the field write
		// on app.downloader / app.downloaderStats still happens.
		_ = app.ReloadDownloader([]config.ServerConfig{})
	})

	time.Sleep(d)
	close(stop)
	wg.Wait()
}

// TestReloadUnderLoad_Race is the red test for issue #98.
//
// It MUST fail under -race on unpatched code. The failure is expected to be
// reported as a data race on app.downloaderStats between ReloadDownloader
// (write, reloader.go) and Speed (read, app.go).
//
// Run it as:
//
//	go test -race -run TestReloadUnderLoad -count=100 ./internal/app/
//	GOMAXPROCS=1 go test -race -run TestReloadUnderLoad -count=100 ./internal/app/
//
// A single green run proves nothing: a race that is not observed is still a
// race. Only the fix (taking app.mu in every reader, or snapshot-then-call)
// makes this reliably green.
func TestReloadUnderLoad_Race(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	fd := newFakeDownloader()

	app, err := New(cfg, nil, WithDownloader(fd))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Shutdown(); err != nil {
			t.Logf("Shutdown: %v", err)
		}
	})

	runReloadUnderLoad(t, app, raceLoadDuration)
}

// reloadMuHoldTestDuration is how long TestReloadDownloader_DoesNotHoldMuAcrossBody
// drives a concurrent ReloadDownloader writer against the app.mu monitor.
const reloadMuHoldTestDuration = 300 * time.Millisecond

// maxAcceptableMuHoldStreak bounds how long app.mu may ever be continuously
// held, as observed by a tight TryLock-polling loop running concurrently with
// ReloadDownloader. On correct code, app.mu is only held for two tiny field
// operations (an initial started/stopped check + snapshot, and the final
// downloader/downloaderStats swap) — measured max ~164µs over 40 runs. On
// unpatched code, app.mu is held for the entire ReloadDownloader body,
// including a real cross-goroutine handshake in pipeline.setCompletions
// (see pipeline.go) plus Stop/ClearAllEmitted/New/Start — measured min ~614µs
// over 40 runs. 2ms gives generous headroom above the measured patched-code
// ceiling for a loaded/slow CI runner (a preempted monitor goroutine or a GC
// pause during the critical section could otherwise push the observed streak
// past a tighter threshold on correct code) while staying well below the
// unpatched-code floor.
const maxAcceptableMuHoldStreak = 2 * time.Millisecond

// TestReloadDownloader_DoesNotHoldMuAcrossBody is the red test for issue
// #118: it must fail on code where ReloadDownloader holds app.mu for its
// entire body (rather than only to snapshot/swap app.downloader).
//
// It measures the lock directly via TryLock polling rather than timing a
// downstream call (e.g. PauseDownloads), which would be confounded by
// unrelated goroutine-scheduling noise.
//
// Run it as:
//
//	go test -race -run TestReloadDownloader_DoesNotHoldMuAcrossBody -count=20 ./internal/app/
func TestReloadDownloader_DoesNotHoldMuAcrossBody(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	fd := newFakeDownloader()

	app, err := New(cfg, nil, WithDownloader(fd))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Shutdown(); err != nil {
			t.Logf("Shutdown: %v", err)
		}
	})

	stop := make(chan struct{})
	var maxHeldStreakNs atomic.Int64
	var wg sync.WaitGroup

	// Monitor: busy-polls TryLock. Each failure means app.mu was held at
	// that instant; a run of consecutive failures approximates one
	// continuous hold. Sampling as fast as possible keeps the
	// under-measurement error small relative to the multi-millisecond
	// streak the unpatched code produces.
	wg.Go(func() {
		var streakStart time.Time
		inStreak := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			if app.mu.TryLock() {
				app.mu.Unlock()
				inStreak = false
				continue
			}
			now := time.Now()
			if !inStreak {
				streakStart = now
				inStreak = true
				continue
			}
			d := now.Sub(streakStart)
			for {
				prev := maxHeldStreakNs.Load()
				if int64(d) <= prev {
					break
				}
				if maxHeldStreakNs.CompareAndSwap(prev, int64(d)) {
					break
				}
			}
		}
	})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = app.ReloadDownloader([]config.ServerConfig{})
			}
		}
	})

	time.Sleep(reloadMuHoldTestDuration)
	close(stop)
	wg.Wait()

	if got := time.Duration(maxHeldStreakNs.Load()); got > maxAcceptableMuHoldStreak {
		t.Errorf("max observed app.mu hold streak = %v, want <= %v (ReloadDownloader appears to hold app.mu across Stop/setCompletions/New/Start)",
			got, maxAcceptableMuHoldStreak)
	}
}

// minAcceptableReloadMuHoldStreak is the floor for the longest streak
// app.reloadMu is observed continuously held while two ReloadDownloader
// callers race for it. ReloadDownloader's own body takes at least a few
// hundred microseconds end to end (the same setCompletions handshake plus
// Stop/ClearAllEmitted/New/Start measured in
// TestReloadDownloader_DoesNotHoldMuAcrossBody), so a healthy reloadMu that
// wraps the whole function must show a streak at least in that range. If a
// future change narrows reloadMu's scope (e.g. moving the Lock call past the
// snapshot, or Unlock-ing before Start), two overlapping callers would only
// contend for a sliver of the function and this streak would collapse well
// below the floor. 100µs (well under the ~614µs measured minimum full-body
// duration) leaves headroom for a faster/quieter machine to still clear the
// floor on correct code.
const minAcceptableReloadMuHoldStreak = 100 * time.Microsecond

// TestReloadDownloader_SerializesConcurrentCalls is the red test for the
// interleaving hazard identified in review of issue #118's fix: narrowing
// app.mu to just the field-swap removed the incidental self-serialization
// the old whole-body app.mu hold provided. Two concurrent ReloadDownloader
// callers could otherwise both build and Start a new downloader and race to
// swap app.downloader / app.pipeline's completions source, leaking the
// loser's downloader and potentially stalling dispatch. reloadMu restores
// serialization independently of app.mu.
//
// It drives two concurrent ReloadDownloader callers and a reloadMu monitor
// (TryLock polling, as in TestReloadDownloader_DoesNotHoldMuAcrossBody) and
// asserts the longest observed continuous hold is long enough to have
// covered a full reload body — proving reloadMu is not just present but
// actually wraps the whole function.
func TestReloadDownloader_SerializesConcurrentCalls(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	fd := newFakeDownloader()

	app, err := New(cfg, nil, WithDownloader(fd))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Shutdown(); err != nil {
			t.Logf("Shutdown: %v", err)
		}
	})

	stop := make(chan struct{})
	var maxHeldStreakNs atomic.Int64
	var wg sync.WaitGroup

	wg.Go(func() {
		var streakStart time.Time
		inStreak := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			if app.reloadMu.TryLock() {
				app.reloadMu.Unlock()
				inStreak = false
				continue
			}
			now := time.Now()
			if !inStreak {
				streakStart = now
				inStreak = true
				continue
			}
			d := now.Sub(streakStart)
			for {
				prev := maxHeldStreakNs.Load()
				if int64(d) <= prev {
					break
				}
				if maxHeldStreakNs.CompareAndSwap(prev, int64(d)) {
					break
				}
			}
		}
	})
	// Two concurrent writers so there is always a second caller contending
	// for reloadMu while the first holds it.
	for range 2 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_ = app.ReloadDownloader([]config.ServerConfig{})
				}
			}
		})
	}

	time.Sleep(reloadMuHoldTestDuration)
	close(stop)
	wg.Wait()

	if got := time.Duration(maxHeldStreakNs.Load()); got < minAcceptableReloadMuHoldStreak {
		t.Errorf("max observed app.reloadMu hold streak = %v, want >= %v (reloadMu does not appear to span ReloadDownloader's full body, so concurrent reloads could interleave)",
			got, minAcceptableReloadMuHoldStreak)
	}
}
