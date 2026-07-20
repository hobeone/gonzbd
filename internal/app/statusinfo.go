package app

import (
	"context"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/config"
)

// BinaryVersions holds the resolved version strings for external
// post-processing tools, captured once at startup. Paths are not
// included here — callers resolve those independently via exec.LookPath
// (see internal/api/about.go's resolveBinary), since path resolution
// doesn't require the startup probe.
type BinaryVersions struct {
	Par2Version   string
	UnrarVersion  string
	SevenzVersion string
}

// BinaryVersionsInfo returns the external tool version strings captured
// by the startup probe. Safe to call from any goroutine.
func (app *Application) BinaryVersionsInfo() BinaryVersions {
	return app.binaryVersions
}

// ArticleCacheBytes returns the current number of bytes buffered in the
// post-processing pipeline's write-coalescing cache. Safe to call from
// any goroutine.
func (app *Application) ArticleCacheBytes() int64 {
	if app.assembler == nil {
		return 0
	}
	return app.assembler.CacheUsageBytes()
}

// downloadDir returns the currently configured download directory path.
func (app *Application) downloadDir() string {
	var dlDir string
	app.config.WithRead(func(c *config.Config) {
		dlDir = c.General.DownloadDir
	})
	return dlDir
}

// DownloadDirFreeBytes returns the free bytes available on the
// filesystem containing the configured download directory. Bounded by
// ctx: statfs has no timeout of its own and can block indefinitely on a
// stuck network mount, so a caller-supplied deadline is required to keep
// a status-page request from hanging.
func (app *Application) DownloadDirFreeBytes(ctx context.Context) (int64, error) {
	return assembler.FreeBytes(ctx, app.downloadDir())
}

// TestDownloadDirWriteSpeedMBPerSec runs a bounded disk write-speed test
// against the configured download directory. Backs the status page's
// on-demand "Test Disk Speed" action.
func (app *Application) TestDownloadDirWriteSpeedMBPerSec(ctx context.Context) (float64, error) {
	const testSizeBytes = 64 * 1024 * 1024 // 64 MiB
	return assembler.WriteSpeedMBPerSec(ctx, app.downloadDir(), testSizeBytes)
}

// RecordHeartbeat records the current unix timestamp as pipeline activity.
func (app *Application) RecordHeartbeat() {
	app.lastHeartbeat.Store(time.Now().Unix())
}

// PingDB verifies history repository database connectivity. Safe when historyRepo is nil.
func (app *Application) PingDB(ctx context.Context) error {
	if app.historyRepo == nil {
		return nil
	}
	return app.historyRepo.Ping(ctx)
}

// IsPipelineHealthy returns true if the application is running and its download/assembly
// pipeline is active and non-stalled.
func (app *Application) IsPipelineHealthy(ctx context.Context) bool {
	if !app.started.Load() || app.stopped.Load() {
		return false
	}
	if app.queue == nil {
		return true
	}
	// Paused queue or active post-processing is considered healthy/active.
	if app.queue.IsPaused() || app.queue.HasPostProcJobs() {
		return true
	}
	// Staleness check applies only when unpaused jobs are actively downloading.
	if app.queue.HasDownloadingJobs() {
		last := app.lastHeartbeat.Load()
		if last > 0 && time.Since(time.Unix(last, 0)) > 2*time.Minute {
			return false // download pipeline stalled
		}
	}
	return true
}
