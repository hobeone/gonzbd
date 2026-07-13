package app

import (
	"context"

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

// DownloadDirFreeBytes returns the free bytes available on the
// filesystem containing the configured download directory.
func (app *Application) DownloadDirFreeBytes() (int64, error) {
	var dlDir string
	app.config.WithRead(func(c *config.Config) {
		dlDir = c.General.DownloadDir
	})
	return assembler.FreeBytes(dlDir)
}

// TestDownloadDirWriteSpeedMBPerSec runs a bounded disk write-speed test
// against the configured download directory. Backs the status page's
// on-demand "Test Disk Speed" action.
func (app *Application) TestDownloadDirWriteSpeedMBPerSec(ctx context.Context) (float64, error) {
	var dlDir string
	app.config.WithRead(func(c *config.Config) {
		dlDir = c.General.DownloadDir
	})
	const testSizeBytes = 64 * 1024 * 1024 // 64 MiB
	return assembler.WriteSpeedMBPerSec(ctx, dlDir, testSizeBytes)
}
