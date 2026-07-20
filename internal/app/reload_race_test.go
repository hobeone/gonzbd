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

// reloadMuHoldTestDuration is how long TestReloadDownloader_SerializesConcurrentCalls
// drives its concurrent ReloadDownloader writers against the reloadMu monitor.
const reloadMuHoldTestDuration = 300 * time.Millisecond

// minAcceptableReloadMuHoldStreak is the floor for the longest streak
// app.reloadMu is observed continuously held while two ReloadDownloader
// callers race for it. ReloadDownloader's own body takes at least a few
// hundred microseconds end to end (the setCompletions cross-goroutine
// handshake plus Stop/ClearAllEmitted/New/Start), so a healthy reloadMu that
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
// (busy-polling TryLock, so a run of consecutive failures approximates one
// continuous hold) and asserts the longest observed continuous hold is long
// enough to have
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
