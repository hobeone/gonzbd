package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

func TestApplication_ReloadOptions(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	fd := newFakeDownloader()
	app, err := New(cfg, nil, WithDownloader(fd))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 1. Test ReloadDownloadOptions happy path
	cfg.With(func(c *config.Config) {
		c.Downloads.MinFreeSpace = config.ByteSize(50 * 1024 * 1024)
		c.Downloads.MaxArtTries = 5
		c.Downloads.MaxArtOpt = 10
		c.Downloads.TopOnly = true
		c.Downloads.PropagationDelay = 30
	})
	app.ReloadDownloadOptions(cfg)

	// Assert on Assembler
	if got := app.assembler.MinFreeBytes(); got != 50*1024*1024 {
		t.Errorf("expected MinFreeBytes to be 50MiB, got %d", got)
	}

	// Assert on fakeDownloader options
	fd.mu.Lock()
	if fd.maxArtTries != 5 {
		t.Errorf("expected maxArtTries to be 5, got %d", fd.maxArtTries)
	}
	if fd.maxArtOpt != 10 {
		t.Errorf("expected maxArtOpt to be 10, got %d", fd.maxArtOpt)
	}
	if !fd.topOnly {
		t.Error("expected topOnly to be true")
	}
	if fd.propagationDelay != 30*time.Minute {
		t.Errorf("expected propagationDelay to be 30m, got %v", fd.propagationDelay)
	}
	fd.mu.Unlock()

	// 2. Test ReloadGeneralOptions (global level)
	cfg.With(func(c *config.Config) {
		c.General.LogLevel = "debug"
		c.General.LogLevels = map[string]string{"downloader": "warn"}
	})
	app.ReloadGeneralOptions(cfg)

	if got := globalLevelVar.Level(); got != slog.LevelDebug {
		t.Errorf("expected global level to be Debug, got %v", got)
	}

	componentLevelsMu.RLock()
	lvl, ok := componentLevels["downloader"]
	componentLevelsMu.RUnlock()
	if !ok || lvl != slog.LevelWarn {
		t.Errorf("expected downloader component level to be Warn, got %v (ok=%t)", lvl, ok)
	}

	// 3. Test ReloadGeneralOptions invalid global level (should log error and NOT apply)
	cfg.With(func(c *config.Config) {
		c.General.LogLevel = "invalid-level"
	})
	app.ReloadGeneralOptions(cfg)
	// Global level should still be debug from previous step
	if got := globalLevelVar.Level(); got != slog.LevelDebug {
		t.Errorf("expected global level to remain Debug on invalid config, got %v", got)
	}

	// 4. Test ReloadGeneralOptions invalid component level (should log error and NOT apply)
	cfg.With(func(c *config.Config) {
		c.General.LogLevel = "debug"
		c.General.LogLevels = map[string]string{"downloader": "invalid-level"}
	})
	app.ReloadGeneralOptions(cfg)
	// Downloader level should still be warn from previous step
	componentLevelsMu.RLock()
	lvl, ok = componentLevels["downloader"]
	componentLevelsMu.RUnlock()
	if !ok || lvl != slog.LevelWarn {
		t.Errorf("expected downloader component level to remain Warn on invalid config, got %v", lvl)
	}

	// 5. Test ReloadGeneralOptions invalid global level but valid component level (partial apply)
	cfg.With(func(c *config.Config) {
		c.General.LogLevel = "invalid-level"
		c.General.LogLevels = map[string]string{"downloader": "info"}
	})
	app.ReloadGeneralOptions(cfg)
	// Global level should remain debug
	if got := globalLevelVar.Level(); got != slog.LevelDebug {
		t.Errorf("expected global level to remain Debug, got %v", got)
	}
	// Downloader level should update to Info
	componentLevelsMu.RLock()
	lvl, ok = componentLevels["downloader"]
	componentLevelsMu.RUnlock()
	if !ok || lvl != slog.LevelInfo {
		t.Errorf("expected downloader component level to update to Info, got %v (ok=%t)", lvl, ok)
	}
}

func TestApplication_RunMetricsPush(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	emitter := &eventCounter{}
	app.SetEmitter(emitter)

	// Widen timeout to 3 seconds to avoid race flake with the 1s ticker.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	app.runMetricsPush(ctx)

	if emitter.count == 0 {
		t.Error("expected metrics push to broadcast events")
	}
}

type eventCounter struct {
	count int
}

func (e *eventCounter) Broadcast(ev Event) {
	e.count++
}
