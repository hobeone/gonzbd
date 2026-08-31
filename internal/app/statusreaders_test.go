package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/downloader"
)

// downloaderOnly implements Downloader but deliberately does not implement
// DownloaderStats, exercising the branch in Speed/ServerStatus that guards
// against a WithDownloader injection lacking stats support. This is a real,
// reachable path through the public constructor API, not dead code: New
// only wires app.downloaderStats when the injected Downloader also satisfies
// DownloaderStats (see WithDownloader).
type downloaderOnly struct{}

func (downloaderOnly) Start(context.Context) error                      { return nil }
func (downloaderOnly) Stop() error                                      { return nil }
func (downloaderOnly) Completions() <-chan *downloader.ArticleResult    { return nil }
func (downloaderOnly) SetSpeedLimit(int64)                              {}
func (downloaderOnly) SetDispatchOptions(int, int, bool, time.Duration) {}
func (downloaderOnly) UnblockServer(string) bool                        { return false }
func (downloaderOnly) Pause()                                           {}
func (downloaderOnly) Resume()                                          {}
func (downloaderOnly) DisconnectAll()                                   {}

// TestSpeed_NilDownloaderStats and TestServerStatus_NilDownloaderStats pin
// the nil-guard branch that //nocover: previously exempted from testing
// (issue #106). Both functions branch on app.downloaderStats == nil, which
// disqualifies them from that exemption under AGENTS.md's own rule, and the
// nil case was untested prior to this change.
func TestSpeed_NilDownloaderStats(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil, WithDownloader(downloaderOnly{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := app.Speed(); got != 0 {
		t.Errorf("Speed() with no DownloaderStats: got %v, want 0", got)
	}
}

func TestServerStatus_NilDownloaderStats(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil, WithDownloader(downloaderOnly{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := app.ServerStatus(); got != nil {
		t.Errorf("ServerStatus() with no DownloaderStats: got %v, want nil", got)
	}
}

// TestSpeed_DelegatesToDownloaderStats and TestServerStatus_Delegates pin the
// non-nil branch: the returned value must be the one the injected
// DownloaderStats implementation provides, not a coincidental zero value.
// fakeDownloader.ServerStatus returns a settable field (not a hardcoded nil)
// specifically so this test can distinguish "delegated and got a value" from
// "the nil-guard fired."
func TestSpeed_DelegatesToDownloaderStats(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	fd := newFakeDownloader()
	fd.speed = 42.5

	app, err := New(cfg, nil, WithDownloader(fd))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := app.Speed(); got != 42.5 {
		t.Errorf("Speed() = %v, want 42.5 (from fakeDownloader)", got)
	}
}

func TestServerStatus_DelegatesToDownloaderStats(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	fd := newFakeDownloader()
	fd.serverStatus = []downloader.ServerSnapshot{{Name: "sentinel-server"}}

	app, err := New(cfg, nil, WithDownloader(fd))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := app.ServerStatus()
	if len(got) != 1 || got[0].Name != "sentinel-server" {
		t.Errorf("ServerStatus() = %+v, want the fakeDownloader's sentinel snapshot", got)
	}
}

// TestHandleLowDisk_NilDownloader pins the nil-guard: the assembler's
// low-disk callback must not panic when app.downloader is nil (e.g. a
// low-disk event racing Application construction, or a fresh &Application{}
// used by other white-box tests in this package). Prior to this change, the
// callback called app.downloader.Pause() unconditionally. New() always wires
// a real downloader when one isn't injected, so this branch is exercised via
// direct struct construction rather than the public constructor, matching
// the pattern used by TestBuildDownloaderOptions_Defaults.
func TestHandleLowDisk_NilDownloader(t *testing.T) {
	t.Parallel()
	app := &Application{log: slog.New(slog.DiscardHandler)}

	app.handleLowDisk("/tmp/somewhere", 1024)
}

// TestHandleLowDisk_PausesDownloader pins the non-nil branch: a real
// downloader must be paused when free disk space drops below threshold.
func TestHandleLowDisk_PausesDownloader(t *testing.T) {
	t.Parallel()
	fd := newFakeDownloader()
	app := &Application{log: slog.New(slog.DiscardHandler), downloader: fd}

	app.handleLowDisk("/tmp/somewhere", 1024)

	fd.mu.Lock()
	paused := fd.paused
	fd.mu.Unlock()
	// --- No lock held below this line ---
	if !paused {
		t.Error("handleLowDisk did not pause the downloader")
	}
}
