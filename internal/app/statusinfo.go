package app

import (
	"context"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
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
	return app.config.GetGeneral().DownloadDir
}

// DownloadDirFreeBytes returns the free bytes available on the
// filesystem containing the configured download directory. Bounded by
// ctx: statfs has no timeout of its own and can block indefinitely on a
// stuck network mount, so a caller-supplied deadline is required to keep
// a status-page request from hanging.
//
// Routed through app.diskProbe (rather than calling assembler.FreeBytes
// directly) because this is called from /health and the status-overview
// API on every HTTP request — without deduping, each poll against a stuck
// mount would abandon its own goroutine forever.
func (app *Application) DownloadDirFreeBytes(ctx context.Context) (int64, error) {
	if app.diskProbe == nil {
		// Defensive fallback for a zero-value Application (not constructed
		// via New()); production code always goes through the cached path.
		return assembler.FreeBytes(ctx, app.downloadDir())
	}
	return app.diskProbe.FreeBytes(ctx, app.downloadDir())
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
		app.RecordHeartbeat()
		return true
	}
	// Staleness check applies only when unpaused jobs are actively downloading.
	if app.queue.HasDownloadingJobs() {
		last := app.lastHeartbeat.Load()
		if last > 0 && time.Since(time.Unix(last, 0)) > 2*time.Minute {
			return false // download pipeline stalled
		}
	} else {
		// Queue is idle: keep heartbeat fresh so download start receives a full grace period.
		app.RecordHeartbeat()
	}
	return true
}

// JobDurability is what R26 asks a job to be able to report at any time: how
// much of it is on stable storage, how much is written but not yet covered by
// an fsync, and when its last barrier succeeded.
//
// The two byte figures are reported separately and are never summed. They make
// different claims — one survives a power loss, the other is the rework window
// B1 bounds — and adding them produces a number that asserts the stronger of
// the two about all of it.
type JobDurability struct {
	// DurableBytes is what a completed fsync covers.
	DurableBytes int64
	// PendingBytes is what has been written since this job's current
	// checkpoint window opened: accepted by the OS, not yet fsynced, and lost
	// on a power failure.
	PendingBytes int64
	// LastBarrier is when this job's last barrier completed without error, or
	// the zero time when none has.
	LastBarrier time.Time
}

// JobDurability reports one job's durability figures. Safe to call from any
// goroutine, and safe at any residency.
//
// DurableBytes comes from the job's downloaded-byte total rather than from a
// counter of its own, because on this design they are the same quantity: the
// only things that mark an article Done are Queue.AckDurable, which takes a
// DurableProof no path outside a completed barrier can mint, and
// SeedFromExtents, which replays a committed Class B cache. A second counter
// would be a second representation of one fact, free to drift (S5).
func (app *Application) JobDurability(jobID string) JobDurability {
	var out JobDurability
	if app.queue != nil {
		if snap := app.queue.SnapshotJob(jobID); snap != nil {
			p := snap.Progress()
			// expected - failed - remaining is the downloaded identity
			// internal/app/history_helper.go already relies on; see
			// JobProgress.ExpectedBytes for why the three legs close.
			out.DurableBytes = p.ExpectedBytes() - p.FailedBytes() - p.RemainingBytes()
		}
	}
	app.barrierMu.Lock()
	out.PendingBytes = app.jobBarrierBytes[jobID]
	out.LastBarrier = app.lastBarrier[jobID]
	app.barrierMu.Unlock()
	return out
}
