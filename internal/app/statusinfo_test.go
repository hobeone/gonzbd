package app

import (
	"context"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/queue"
)

func TestApplication_ArticleCacheBytes_ReturnsZeroInitially(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	if got := app.ArticleCacheBytes(); got != 0 {
		t.Errorf("ArticleCacheBytes() = %d, want 0 on a fresh app", got)
	}
}

func TestApplication_DownloadDirFreeBytes_ReturnsPositiveForRealDir(t *testing.T) {
	dlDir := t.TempDir()
	cfg := testConfig(dlDir, t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	free, err := app.DownloadDirFreeBytes(t.Context())
	if err != nil {
		t.Fatalf("DownloadDirFreeBytes: %v", err)
	}
	if free <= 0 {
		t.Errorf("DownloadDirFreeBytes() = %d, want > 0 for a real temp dir", free)
	}
}

func TestApplication_TestDownloadDirWriteSpeedMBPerSec_ReturnsPositive(t *testing.T) {
	dlDir := t.TempDir()
	cfg := testConfig(dlDir, t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	mbPerSec, err := app.TestDownloadDirWriteSpeedMBPerSec(context.Background())
	if err != nil {
		t.Fatalf("TestDownloadDirWriteSpeedMBPerSec: %v", err)
	}
	if mbPerSec <= 0 {
		t.Errorf("mbPerSec = %f, want > 0", mbPerSec)
	}
}

func TestApplication_BinaryVersionsInfo_StableAcrossCalls(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	// Whatever par2/unrar/7z are (or aren't) installed on the test
	// machine, BinaryVersionsInfo() must return the same retained
	// struct every call — proving it reads stored state from New()'s
	// probe rather than re-probing (which would be slow and wasteful)
	// or returning something nondeterministic.
	first := app.BinaryVersionsInfo()
	second := app.BinaryVersionsInfo()
	if first != second {
		t.Errorf("BinaryVersionsInfo() not stable across calls: %+v vs %+v", first, second)
	}
}

func TestApplication_IsPipelineHealthy(t *testing.T) {
	dlDir := t.TempDir()
	cfg := testConfig(dlDir, t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()

	// Unstarted app should report unhealthy
	if app.IsPipelineHealthy(ctx) {
		t.Error("expected IsPipelineHealthy=false for unstarted app")
	}

	if err := app.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer app.Shutdown()

	// Idle app with empty queue should report healthy
	if !app.IsPipelineHealthy(ctx) {
		t.Error("expected IsPipelineHealthy=true for started idle app")
	}

	// PingDB should succeed on nil historyRepo
	if err := app.PingDB(ctx); err != nil {
		t.Errorf("PingDB on nil historyRepo: %v", err)
	}

	// Test RecordHeartbeat updates timestamp
	app.RecordHeartbeat()
	if app.lastHeartbeat.Load() <= 0 {
		t.Error("expected lastHeartbeat > 0 after RecordHeartbeat")
	}

	// Test download pipeline stall detection
	j := &queue.Job{ID: "job1", Status: constants.StatusDownloading}
	if err := app.queue.Add(j); err != nil {
		t.Fatalf("queue Add: %v", err)
	}

	// Set heartbeat to 3 minutes ago
	app.lastHeartbeat.Store(time.Now().Add(-3 * time.Minute).Unix())
	if app.IsPipelineHealthy(ctx) {
		t.Error("expected IsPipelineHealthy=false for stalled download pipeline (>2m heartbeat)")
	}

	// Update heartbeat to now
	app.RecordHeartbeat()
	if !app.IsPipelineHealthy(ctx) {
		t.Error("expected IsPipelineHealthy=true after fresh heartbeat")
	}
}
