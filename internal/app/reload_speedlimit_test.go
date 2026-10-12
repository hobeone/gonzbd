package app

import (
	"context"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

// startedLimitedApp returns a started Application whose config caps the
// download rate at bandwidth_max=1000000 x bandwidth_perc=50.
func startedLimitedApp(t *testing.T) *Application {
	t.Helper()
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	cfg.With(func(c *config.Config) {
		c.Downloads.BandwidthMax = 1000000
		c.Downloads.BandwidthPerc = 50
	})
	application, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })
	return application
}

// installedSpeedLimit reads the rate cap of the downloader app currently runs.
func installedSpeedLimit(app *Application) int64 {
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.downloaderStats.SpeedLimit()
}

// TestReloadDownloader_KeepsTheConfiguredSpeedLimit: a server edit rebuilds
// the downloader, and the new one must run at the cap the config sets, not
// unlimited.
func TestReloadDownloader_KeepsTheConfiguredSpeedLimit(t *testing.T) {
	t.Parallel()
	application := startedLimitedApp(t)
	if got := installedSpeedLimit(application); got != 500000 {
		t.Fatalf("SpeedLimit before the reload = %d, want 500000", got)
	}
	if err := application.ReloadDownloader(nil); err != nil {
		t.Fatalf("ReloadDownloader: %v", err)
	}
	if got := installedSpeedLimit(application); got != 500000 {
		t.Errorf("SpeedLimit after the reload = %d, want 500000: the rebuilt downloader "+
			"dropped the configured cap", got)
	}
}

// TestReloadDownloader_KeepsASpeedLimitSetAtRuntime: a limit set through
// SetSpeedLimit (the API's speedlimit mode and its bandwidth edits) is the one
// in force, and a reload must carry it rather than fall back to the config's.
func TestReloadDownloader_KeepsASpeedLimitSetAtRuntime(t *testing.T) {
	t.Parallel()
	application := startedLimitedApp(t)
	application.SetSpeedLimit(123456)
	if err := application.ReloadDownloader(nil); err != nil {
		t.Fatalf("ReloadDownloader: %v", err)
	}
	if got := installedSpeedLimit(application); got != 123456 {
		t.Errorf("SpeedLimit after the reload = %d, want 123456: the rebuilt downloader "+
			"did not keep the limit set at runtime", got)
	}
}
