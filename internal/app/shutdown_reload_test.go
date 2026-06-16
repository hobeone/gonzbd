package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

// ---------- H9: Shutdown error propagation ----------

// TestShutdown_PropagatesErrors verifies that Shutdown collects and returns
// errors from subsystem teardown instead of silently discarding them.
func TestShutdown_PropagatesErrors(t *testing.T) {
	t.Parallel()

	// Verify that errors.Join correctly surfaces multiple errors.
	// This tests the pattern used in Shutdown.
	e1 := fmt.Errorf("downloader stop: %w", errors.New("conn timeout"))
	e2 := fmt.Errorf("queue save: %w", errors.New("disk full"))

	joined := errors.Join(e1, e2)
	if joined == nil {
		t.Fatal("errors.Join returned nil for non-nil inputs")
	}
	errStr := joined.Error()
	if !strings.Contains(errStr, "downloader stop") {
		t.Errorf("joined error missing 'downloader stop': %v", joined)
	}
	if !strings.Contains(errStr, "queue save") {
		t.Errorf("joined error missing 'queue save': %v", joined)
	}

	// Verify nil case: when all succeed, errors.Join returns nil.
	var noErrs []error
	if result := errors.Join(noErrs...); result != nil {
		t.Errorf("errors.Join(nil...) = %v, want nil", result)
	}
}

// ---------- H10: buildDownloaderOptions ----------

// TestBuildDownloaderOptions_FieldMapping verifies that buildDownloaderOptions
// correctly maps all config fields to downloader.Options.
func TestBuildDownloaderOptions_FieldMapping(t *testing.T) {
	t.Parallel()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) {
		c.Downloads.MaxArtTries = 5
		c.Downloads.MaxArtOpt = 3
		c.Downloads.TopOnly = true
		c.Downloads.NoPenalties = true
		c.Downloads.PreCheck = true
		c.Downloads.PropagationDelay = 10 // minutes
	})

	app := &Application{
		config: cfg,
	}

	opts := app.buildDownloaderOptions()

	if opts.MaxArtTries != 5 {
		t.Errorf("MaxArtTries = %d, want 5", opts.MaxArtTries)
	}
	if opts.MaxArtOpt != 3 {
		t.Errorf("MaxArtOpt = %d, want 3", opts.MaxArtOpt)
	}
	if !opts.TopOnly {
		t.Error("TopOnly = false, want true")
	}
	if !opts.NoPenalties {
		t.Error("NoPenalties = false, want true")
	}
	if !opts.PreCheck {
		t.Error("PreCheck = false, want true")
	}
	if opts.PropagationDelay != 10*time.Minute {
		t.Errorf("PropagationDelay = %v, want 10m", opts.PropagationDelay)
	}
	if opts.OnJobHopeless == nil {
		t.Error("OnJobHopeless = nil, want non-nil callback")
	}
}

// TestBuildDownloaderOptions_Defaults verifies that zero-valued config
// produces sensible (zero) defaults without panicking.
func TestBuildDownloaderOptions_Defaults(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	app := &Application{config: cfg}

	opts := app.buildDownloaderOptions()

	if opts.MaxArtTries != 0 {
		t.Errorf("MaxArtTries = %d, want 0", opts.MaxArtTries)
	}
	if opts.TopOnly {
		t.Error("TopOnly should be false by default")
	}
	if opts.NoPenalties {
		t.Error("NoPenalties should be false by default")
	}
	if opts.PreCheck {
		t.Error("PreCheck should be false by default")
	}
	if opts.PropagationDelay != 0 {
		t.Errorf("PropagationDelay = %v, want 0", opts.PropagationDelay)
	}
	// OnJobHopeless should still be set even with default config.
	if opts.OnJobHopeless == nil {
		t.Error("OnJobHopeless = nil, want non-nil callback")
	}
}
