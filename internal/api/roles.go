package api

import (
	"context"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/queue"
)

// The role interfaces below split the former monolithic ApplicationReloader
// into the four independent concerns the API server actually consumes. Each
// handler depends only on the role(s) it uses; the concrete *app.Application
// satisfies all four. Signatures are copied verbatim from the methods
// Application already exposes — this grouping introduces no behaviour or type
// change.

// JobManager covers queue and history job lifecycle operations.
type JobManager interface {
	AddJob(ctx context.Context, job *queue.Job, rawNZB []byte, force bool) error
	RemoveJob(ctx context.Context, id string, deleteFiles bool) error
	RemoveHistoryJob(ctx context.Context, id string, deleteFiles bool) error
	RetryHistoryJob(ctx context.Context, jobID string) error
}

// DownloaderControl covers live control of the downloader: speed/bandwidth
// limits, pause/resume, directory changes, and per-server actions.
type DownloaderControl interface {
	SetSpeedLimit(bytesPerSec int64)
	SetBandwidthMax(bytesPerSec int64)
	SetBandwidthPerc(perc int)
	SetDownloadDir(dir string)
	SetCompleteDir(dir string)
	PauseDownloads()
	ResumeDownloads()
	DisconnectAll()
	UnblockServer(name string) bool
	// ReevaluateStalls asks the application to re-check every job parked by a
	// storage fault, now rather than at its next interval. This is R19's "and
	// on user action": a user who has just cleared a full disk and pressed
	// resume should not wait out the interval as well. Non-blocking.
	ReevaluateStalls()
}

// ConfigReloader covers hot-reload of configuration subsections.
type ConfigReloader interface {
	ReloadDownloader(scs []config.ServerConfig) error
	ReloadPostProcOptions(pp config.PostProcConfig, scriptDir string)
	// ReloadDownloadOptions applies all hot-applicable download settings.
	// Same locking note as ReloadPostProcOptions.
	ReloadDownloadOptions(d config.DownloadConfig)
	// ReloadGeneralOptions applies all hot-applicable general settings.
	// Same locking note as ReloadPostProcOptions.
	ReloadGeneralOptions(g config.GeneralConfig)
}

// StatusReporter covers read-only status and health queries for the status and
// health endpoints.
type StatusReporter interface {
	// Speed returns the current aggregate download speed in bytes/sec.
	Speed() float64
	ServerStatus() []downloader.ServerSnapshot
	// ArticleCacheBytes returns current write-cache usage, for the status page.
	ArticleCacheBytes() int64
	DirectUnpackStatus(jobID string) (directunpack.Status, bool)
	// DirectUnpackStatuses returns a snapshot of every active direct-unpack
	// job's status, keyed by job ID. Used by queueList to take the
	// application-wide mutex once per request instead of once per job
	// (OPT-12).
	DirectUnpackStatuses() map[string]directunpack.Status
	// CheckpointStates returns every job's pending bytes, last-barrier time
	// and stall reason, keyed by job ID. Snapshotted once per request for the
	// same reason DirectUnpackStatuses is.
	CheckpointStates() map[string]app.JobCheckpointState
	// CheckpointState returns one job's figures, for the single-job detail
	// endpoint. Separate from the map above so the drawer does not build a
	// snapshot of every job in the queue to read one entry out of it.
	CheckpointState(jobID string) app.JobCheckpointState
	// BinaryVersionsInfo returns resolved external-tool version strings
	// captured at startup, for the status page.
	BinaryVersionsInfo() app.BinaryVersions
	// DownloadDirFreeBytes returns free disk space on the download directory.
	DownloadDirFreeBytes(ctx context.Context) (int64, error)
	// TestDownloadDirWriteSpeedMBPerSec runs an on-demand disk write-speed test.
	TestDownloadDirWriteSpeedMBPerSec(ctx context.Context) (float64, error)
	// PingDB verifies history database connectivity.
	PingDB(ctx context.Context) error
	// IsPipelineHealthy returns true if the application and pipeline are non-stalled.
	IsPipelineHealthy(ctx context.Context) bool
	// TestNNTPServer dials an NNTP server to verify connectivity and credentials.
	TestNNTPServer(ctx context.Context, cfg config.ServerConfig) (app.NNTPTestResult, error)
}

// AppServices is the aggregate of every role the API server needs from the
// top-level application. It exists solely as the construction-time input type
// (Options.App and the Server fan-out): a single *app.Application provides all
// four roles and tests inject a single fake, so the roles must meet in one
// type at the boundary. Handlers never depend on AppServices directly — they
// depend on the narrow role interfaces above.
type AppServices interface {
	JobManager
	DownloaderControl
	ConfigReloader
	StatusReporter
}

// Compile-time proof that the concrete application satisfies every role (and
// therefore the aggregate). These are the behaviour-free "test" that the split
// preserved the full method set.
var (
	_ JobManager        = (*app.Application)(nil)
	_ DownloaderControl = (*app.Application)(nil)
	_ ConfigReloader    = (*app.Application)(nil)
	_ StatusReporter    = (*app.Application)(nil)
	_ AppServices       = (*app.Application)(nil)
)
