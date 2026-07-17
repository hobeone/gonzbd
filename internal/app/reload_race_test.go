package app

import (
	"sync"
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
