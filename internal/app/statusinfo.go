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
// the application rather than in the queue: what a barrier has not yet covered,
// when the last one succeeded, and why the job is parked.
//
// Separate from JobDurability because the queue listing already holds every
// job's progress and must not re-snapshot it — a listing is polled
// continuously and Queue.SnapshotJob deep-copies and can hydrate a manifest
// from disk. A struct with a DurableBytes field left zero on that path would
// be a figure that silently means two things.
type JobCheckpointState struct {
	// PendingBytes is what has been written since this job's current
	// checkpoint window opened: accepted by the OS, not yet fsynced, and lost
	// on a power failure.
	//
	// DECODED bytes -- the accumulator is fed len(data) per accepted article,
	// because B1's volume bound measures rework at risk and rework is
	// measured in bytes on disk. DurableBytesOf is in ENCODED bytes. The two
	// are not comparable; see the bytes_durable/bytes_pending field doc in
	// internal/api/queue.go for why neither can move to the other's unit.
	PendingBytes int64
	// LastBarrier is when this job's last barrier completed without error, or
	// the zero time when none has.
	LastBarrier time.Time
	// StallReason is the surfaced, actionable text R27 requires, or "" when
	// the job is not parked.
	StallReason string
}

// JobDurability is what R26 asks a job to be able to report at any time: how
// much of it is on stable storage, how much is written but not yet covered by
// an fsync, when its last barrier succeeded, and why it is parked.
//
// The two byte figures are reported separately and are never summed. They make
// different claims — one survives a power loss, the other is the rework window
// B1 bounds — and adding them produces a number that asserts the stronger of
// the two about all of it.
type JobDurability struct {
	JobCheckpointState
	// DurableBytes is what a completed fsync covers.
	DurableBytes int64
}

// CheckpointState reads one job's application-side figures, for the single-job
// detail endpoint. The whole-queue listing uses CheckpointStates instead.
func (app *Application) CheckpointState(jobID string) JobCheckpointState {
	var out JobCheckpointState
	app.barrierMu.Lock()
	out.PendingBytes = app.jobBarrierBytes[jobID]
	out.LastBarrier = app.lastBarrier[jobID]
	app.barrierMu.Unlock()
	out.StallReason = app.StallReason(jobID).Reason
	return out
}

// CheckpointStates returns every job that has a figure worth reporting, keyed
// by job ID.
//
// One pass under each lock rather than one pass per job, for the same reason
// DirectUnpackStatuses exists: the queue listing is polled continuously, and
// taking these locks per slot would put every poll's cost on the write path's
// contention.
func (app *Application) CheckpointStates() map[string]JobCheckpointState {
	out := make(map[string]JobCheckpointState)
	app.barrierMu.Lock()
	for jobID, n := range app.jobBarrierBytes {
		st := out[jobID]
		st.PendingBytes = n
		out[jobID] = st
	}
	for jobID, at := range app.lastBarrier {
		st := out[jobID]
		st.LastBarrier = at
		out[jobID] = st
	}
	app.barrierMu.Unlock()

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

// JobDurability reports one job's durability figures. Safe to call from any
// goroutine, and safe at any residency.
//
// DurableBytes comes from the job's downloaded-byte total rather than from a
// counter of its own, because on this design they are the same quantity.
// Everything that marks an article Done ultimately stands on a barrier's
// fsync: Queue.AckDurable, which takes a DurableProof no path outside a
// completed barrier can mint and hands its articles to queue.ackDurable; the
// two seeding entry points — Queue.SeedFromRuns, which replays the runs a
// barrier recorded through queue.seedFromRuns, and ReplaceFromRuns, which
// installs what the startup sweep's stat left standing; and
// queue.applyResolution, which replays the resolution derived from those same
// records when a job is re-hydrated. The two markDone calls behind the first
// two entry points moved onto unexported *Job methods in B2.4a; the entry
// points and the evidence they require are unchanged.
// queue.markFailed sets the bit too, for an
// article whose bytes will never arrive and which therefore contributes no
// downloaded bytes.
//
// One path sets the bit WITHOUT going through markDone at all, and it is named
// here rather than left to the word "ultimately": queue.newJobProgressSized
// writes p.done directly when sizing a non-resident job's progress, because
// markDone needs a manifest for byte arithmetic that has already been seeded
// from job_files. Its input is the same derived resolution — done means
// covered by a durable run — so the identity survives it, but a reader
// grepping for markDone will not find it.
//
// So the identity holds at every residency, and a second counter would be a
// second representation of one fact, free to drift (S5).
//
// See queue.jobProgressJSON, which states the markDone-scoped version of this
// at the bit itself, and TestJobDurability_ReportsDownloadedBytesAsDurable,
// which pins the identity and restates the enumeration above; keep all three
// in step. A narrowing here that says "only the barrier" belongs in none of
// them — this list is the reason why.
//
// Keeping them in step is no longer left to whoever remembers to look:
// queue.TestDoneBitWriters_MatchTheEnumerationStatedInProse parses the queue
// package and fails when the set of functions reaching markDone, or setting
// the bit directly, stops matching what these three sites say. It exists
// because this enumeration was found short TWICE — the second time here,
// months after the copy in queue/progress.go was corrected, because a grep of
// internal/queue cannot reach internal/app.
//
// ReplaceFromRuns also UN-marks an article whose run the resume discarded
// (#362), and this figure follows it down rather than needing a correction of
// its own — which is the same property, stated for the direction the design
// added last.
func (app *Application) JobDurability(jobID string) JobDurability {
	out := JobDurability{JobCheckpointState: app.CheckpointState(jobID)}
	if app.dispatcher != nil {
		if j, ok := app.dispatcher.Job(jobID); ok {
			out.DurableBytes = DurableBytesOf(j)
			return out
		}
	}
	return out
}

// ProgressByteCounters is the subset of JobProgress that DurableBytesOf requires.
type ProgressByteCounters interface {
	ExpectedBytes() int64
	FailedBytes() int64
	RemainingBytes() int64
	ContentFailedBytes() int64
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
// the DECODED payload bytes an fsync proved -- docs/queue-lifecycle.md records
// that substitution overstating every non-resident job's remaining bytes by
// the encoding overhead.
func DurableBytesOf(p ProgressByteCounters) int64 {
	if p == nil {
		return 0
	}
	return p.ExpectedBytes() - p.FailedBytes() - p.RemainingBytes()
}
