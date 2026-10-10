package app

import (
	"context"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/job"
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
//
//testdouble:allow SABnzbd test_download_dir_write_speed API implementation
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
	if app.dispatcher == nil {
		return true
	}
	if app.dispatcher.Paused() {
		app.RecordHeartbeat()
		return true
	}
	rows := app.dispatcher.List()
	hasPostProc := false
	hasDownloading := false
	for _, r := range rows {
		switch r.View.State {
		case job.Repairing, job.Extracting, job.Finalizing:
			hasPostProc = true
		case job.Fetching:
			hasDownloading = true
		}
	}
	if hasPostProc {
		app.RecordHeartbeat()
		return true
	}
	if hasDownloading {
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

// JobCheckpointState is the part of a job's durability figures that lives in
// the application rather than in the queue: why the job is parked.
//
// The queue listing already holds every job's progress, so the durable figure
// is derived from that and this struct carries only what the application holds.
type JobCheckpointState struct {
	// StallReason is the surfaced, actionable text R27 requires, or "" when
	// the job is not parked.
	StallReason string
}

// CheckpointState reads one job's application-side figures, for the single-job
// detail endpoint. The whole-queue listing uses CheckpointStates instead.
func (app *Application) CheckpointState(jobID string) JobCheckpointState {
	return JobCheckpointState{StallReason: app.StallReason(jobID).Reason}
}

// CheckpointStates returns every parked job, keyed by job ID.
//
// One pass under the stall lock rather than one lock per job, for the same
// reason DirectUnpackStatuses exists: the queue listing is polled continuously.
func (app *Application) CheckpointStates() map[string]JobCheckpointState {
	out := make(map[string]JobCheckpointState)
	app.stallMu.Lock()
	for jobID, rec := range app.stalls {
		if rec.reason == "" {
			continue
		}
		st := out[jobID]
		st.StallReason = rec.reason
		out[jobID] = st
	}
	app.stallMu.Unlock()
	return out
}

// ProgressByteCounters is the subset of JobProgress that DurableBytesOf requires.
type ProgressByteCounters interface {
	ProgressFigures() (expected, remaining, failed int64)
}

// DurableBytesOf derives a job's durable byte total from its progress.
//
// expected - failed - remaining is the downloaded identity
// internal/app/history_helper.go already relies on; see
// JobProgress.ExpectedBytes for why the three legs close. It is exported so
// the queue listing can apply it to the job clone it already holds instead of
// taking a second snapshot per poll.
//
// All three legs are NZB-declared, yEnc-ENCODED bytes, so this figure is too.
// It is deliberately not a sum over the durability record's lengths, which are
// the DECODED payload bytes an fsync proved -- docs/job-lifecycle.md records
// that substitution overstating every non-resident job's remaining bytes by
// the encoding overhead.
func DurableBytesOf(p ProgressByteCounters) int64 {
	if p == nil {
		return 0
	}
	expected, remaining, failed := p.ProgressFigures()
	durable := expected - failed - remaining
	if durable < 0 {
		return 0
	}
	return durable
}
