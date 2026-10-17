package app

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

// TestHandleLowDisk_WarnsOncePerTransition pins that the "downloads paused"
// warning marks the transition into the low-disk pause. The assembler calls
// OnLowDisk every diskCheckInterval writes for as long as free space stays
// below the floor, so a warning per call floods the log. A user pause already
// in force still gets the one warning: the low-disk condition is not otherwise
// reported.
func TestHandleLowDisk_WarnsOncePerTransition(t *testing.T) {
	t.Parallel()

	dlDir := t.TempDir()
	cfg := testConfig(dlDir, t.TempDir(), t.TempDir())
	cfg.With(func(c *config.Config) { c.Downloads.MinFreeSpace = config.ByteSize(1000) })

	var logs lockedBuffer
	application, err := New(cfg, nil,
		WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))),
		WithDownloader(newFakeDownloader()),
		WithDiskProbe(newFakeDiskProbe(500)),
		// Long, so the auto-resume watch never lifts the pause mid-test.
		WithLowDiskRecheckInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := application.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })

	const warning = "low disk space, downloads paused"

	for range 5 {
		application.handleLowDisk(dlDir, 500)
	}
	if got := strings.Count(logs.String(), warning); got != 1 {
		t.Fatalf("warnings after 5 calls in one pause = %d, want 1", got)
	}

	application.ResumeDownloads()
	for range 5 {
		application.handleLowDisk(dlDir, 400)
	}
	if got := strings.Count(logs.String(), warning); got != 2 {
		t.Fatalf("warnings after a resume and 5 more calls = %d, want 2 (one per transition)", got)
	}

	application.ResumeDownloads()
	application.PauseDownloads()
	for range 5 {
		application.handleLowDisk(dlDir, 300)
	}
	if got := strings.Count(logs.String(), warning); got != 3 {
		t.Fatalf("warnings after a user pause and 5 calls = %d, want 3 (a user pause must not hide the condition)", got)
	}
}
