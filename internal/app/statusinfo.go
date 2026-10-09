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

// ArticleCacheBytes returns the number of bytes the assembler buffers in memory
// ahead of the disk. The assembler writes every accepted article synchronously
// and holds no such buffer, so this is always zero; it stays because the status
// overview API and UI still report the field.
func (app *Application) ArticleCacheBytes() int64 {
	return 0
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
// Separate from JobDurability because the queue listing already holds every
// job's progress and must not re-snapshot it — a listing is polled
// continuously and re-copying every job's progress is the cost.
type JobCheckpointState struct {
	// StallReason is the surfaced, actionable text R27 requires, or "" when
	// the job is not parked.
	StallReason string
}

// JobDurability is what R26 asks a job to be able to report at any time: how
// much of it is on stable storage and why it is parked.
type JobDurability struct {
	JobCheckpointState
	// DurableBytes is what a completed fsync covers.
	DurableBytes int64
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

// JobDurability reports one job's durability figures. Safe to call from any
// goroutine, and safe at any residency.
//
// DurableBytes comes from the job's downloaded-byte total rather than from a
// counter of its own, because on this design they are the same quantity.
// Everything that marks an article Done ultimately stands on a barrier's
// fsync: Job.AckDurable, which takes a DurableProof no path outside a
// completed barrier can mint and hands its articles to job.AckDurable; the
// two seeding entry points — Job.SeedFromRuns, which replays the runs a
// barrier recorded through job.SeedFromRuns, and ReplaceFromRuns, which
// installs what the startup sweep's stat left standing; and
// job.ApplyResolution, which replays the resolution derived from those same
// records when a job is re-hydrated. The two markDone calls behind the first
// two entry points moved onto unexported *Job methods in B2.4a; the entry
// points and the evidence they require are unchanged.
// job.setFailedBits sets the bit too, for an article whose bytes will never
// arrive and which therefore contributes no downloaded bytes — through
// job.markFailed, or directly from Job.MarkArticleFailed while the manifest is
// evicted.
//
// One path sets the bit WITHOUT going through markDone at all, and it is named
// here rather than left to the word "ultimately": job.newJobProgressSized
// writes p.done directly when sizing a non-resident job's progress, because
// markDone needs a manifest for byte arithmetic that has already been seeded
// from job_files. Its input is the same derived resolution — done means
// covered by a durable run — so the identity survives it, but a reader
// grepping for markDone will not find it.
//
// So the identity holds at every residency, and a second counter would be a
// second representation of one fact, free to drift (S5).
//
// See job.jobProgressJSON, which states the markDone-scoped version of this
// at the bit itself, and TestJobDurability_ReportsDownloadedBytesAsDurable,
// which pins the identity and restates the enumeration above; keep all three
// in step. A narrowing here that says "only the barrier" belongs in none of
// them — this list is the reason why.
//
// Keeping them in step is no longer left to whoever remembers to look:
// job.TestDoneBitWriters_MatchTheEnumerationStatedInProse parses the job
// package and fails when the set of functions reaching markDone, or setting
// the bit directly, stops matching what these three sites say. It exists
// because this enumeration was found short TWICE — the second time here,
// months after the sibling copy was corrected in the since-deleted
// internal/queue, because a grep of that package could not reach
// internal/app.
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
