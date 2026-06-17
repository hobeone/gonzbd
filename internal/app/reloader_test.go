package app

import (
	"context"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

func TestApplication_ReloadOptions(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cfg.With(func(c *config.Config) {
		c.Downloads.MinFreeSpace = config.ByteSize(50 * 1024 * 1024)
		c.Downloads.MaxArtTries = 5
	})
	app.ReloadDownloadOptions(cfg)

	cfg.With(func(c *config.Config) {
		c.General.LogLevel = "debug"
	})
	app.ReloadGeneralOptions(cfg)

	// Invalid global level
	cfg.With(func(c *config.Config) {
		c.General.LogLevel = "invalid-level"
	})
	app.ReloadGeneralOptions(cfg)

	// Invalid component level
	cfg.With(func(c *config.Config) {
		c.General.LogLevel = "debug"
		c.General.LogLevels = map[string]string{"downloader": "invalid-level"}
	})
	app.ReloadGeneralOptions(cfg)
}

func TestApplication_RunMetricsPush(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	emitter := &eventCounter{}
	app.SetEmitter(emitter)

	ctx, cancel := context.WithTimeout(t.Context(), 1200*time.Millisecond)
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
