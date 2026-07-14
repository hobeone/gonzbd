package apitest

import (
	"context"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

func TestNopAppCoverage(t *testing.T) {
	app := NopApp{
		SpeedVal: 12.34,
	}

	_ = app.ReloadDownloader(nil)
	_ = app.RetryHistoryJob(context.Background(), "job1")
	app.SetSpeedLimit(100)
	app.SetBandwidthMax(200)
	app.SetBandwidthPerc(50)
	app.SetDownloadDir("/tmp/down")
	app.SetCompleteDir("/tmp/comp")
	app.PauseDownloads()
	app.ResumeDownloads()
	app.DisconnectAll()
	app.ReloadPostProcOptions(config.PostProcConfig{}, "/tmp/scripts")
	app.ReloadDownloadOptions(config.DownloadConfig{})
	app.ReloadGeneralOptions(config.GeneralConfig{})
	_ = app.UnblockServer("srv1")
	_ = app.ServerStatus()
	if sp := app.Speed(); sp != 12.34 {
		t.Errorf("Speed() = %v, want 12.34", sp)
	}

	// Test with nil Queue/History (safe paths)
	_ = app.AddJob(context.Background(), nil, nil, false)
	_ = app.RemoveJob(context.Background(), "job1", false)
	_ = app.RemoveHistoryJob(context.Background(), "job1", false)
}
