// Package apitest provides test doubles and stubs for the api package.
package apitest

import (
	"context"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/queue"
)

// NopApp is a no-op implementation of ApplicationReloader.
// It is intended for use in tests (unit, integration, and UI) to eliminate
// duplicate mock definitions of the 20+-method interface.
type NopApp struct {
	Queue              *queue.Queue
	History            *history.Repository
	SpeedVal           float64
	ServerSnapshotsVal []downloader.ServerSnapshot
}

// ReloadDownloader is a stub.
func (n NopApp) ReloadDownloader([]config.ServerConfig) error { return nil }

// RetryHistoryJob is a stub.
func (n NopApp) RetryHistoryJob(context.Context, string) error { return nil }

// SetSpeedLimit is a stub.
func (n NopApp) SetSpeedLimit(int64) {}

// SetBandwidthMax is a stub.
func (n NopApp) SetBandwidthMax(int64) {}

// SetBandwidthPerc is a stub.
func (n NopApp) SetBandwidthPerc(int) {}

// SetDownloadDir is a stub.
func (n NopApp) SetDownloadDir(string) {}

// SetCompleteDir is a stub.
func (n NopApp) SetCompleteDir(string) {}

// PauseDownloads is a stub.
func (n NopApp) PauseDownloads() {}

// ResumeDownloads is a stub.
func (n NopApp) ResumeDownloads() {}

// DisconnectAll is a stub.
func (n NopApp) DisconnectAll() {}

// ReloadPostProcOptions is a stub.
func (n NopApp) ReloadPostProcOptions(config.PostProcConfig, string) {}

// ReloadDownloadOptions is a stub.
func (n NopApp) ReloadDownloadOptions(config.DownloadConfig) {}

// ReloadGeneralOptions is a stub.
func (n NopApp) ReloadGeneralOptions(config.GeneralConfig) {}

// UnblockServer is a stub.
func (n NopApp) UnblockServer(string) bool { return true }

// ServerStatus returns the configured mock server snapshots.
func (n NopApp) ServerStatus() []downloader.ServerSnapshot { return n.ServerSnapshotsVal }

// Speed returns the configured mock speed value.
func (n NopApp) Speed() float64 { return n.SpeedVal }

// AddJob forwards the job adding to the wired queue if present.
func (n NopApp) AddJob(ctx context.Context, job *queue.Job, rawNZB []byte, force bool) error {
	if n.Queue != nil {
		return n.Queue.Add(job)
	}
	return nil
}

// RemoveJob forwards the job removal to the wired queue if present.
func (n NopApp) RemoveJob(ctx context.Context, id string, deleteFiles bool) error {
	if n.Queue != nil {
		return n.Queue.Remove(id)
	}
	return nil
}

// RemoveHistoryJob forwards the history job deletion to the wired repository if present.
func (n NopApp) RemoveHistoryJob(ctx context.Context, id string, deleteFiles bool) error {
	if n.History != nil {
		_, err := n.History.Delete(ctx, id)
		return err
	}
	return nil
}

// DirectUnpackStatus is a stub.
func (n NopApp) DirectUnpackStatus(jobID string) (directunpack.Status, bool) {
	return directunpack.Status{}, false
}

// DirectUnpackStatuses is a stub.
func (n NopApp) DirectUnpackStatuses() map[string]directunpack.Status { //nocover: no-op interface stub
	return nil
}

// BinaryVersionsInfo is a stub.
func (n NopApp) BinaryVersionsInfo() app.BinaryVersions { return app.BinaryVersions{} }

// ArticleCacheBytes is a stub.
func (n NopApp) ArticleCacheBytes() int64 { return 0 }

// DownloadDirFreeBytes is a stub.
func (n NopApp) DownloadDirFreeBytes(context.Context) (int64, error) { return 0, nil }

// TestDownloadDirWriteSpeedMBPerSec is a stub.
func (n NopApp) TestDownloadDirWriteSpeedMBPerSec(context.Context) (float64, error) { return 0, nil }

// PingDB is a stub.
func (n NopApp) PingDB(context.Context) error { return nil }

// IsPipelineHealthy is a stub.
func (n NopApp) IsPipelineHealthy(context.Context) bool { return true }
