package apitest

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	appkg "github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

func TestNopApp_Contract(t *testing.T) {
	ctx := context.Background()
	snaps := []downloader.ServerSnapshot{{Name: "srv1", ActiveConns: 2}}
	app := NopApp{
		SpeedVal:           12.34,
		ServerSnapshotsVal: snaps,
	}

	// 1. Verify stub method return values
	if err := app.ReloadDownloader(nil); err != nil {
		t.Errorf("ReloadDownloader() = %v, want nil", err)
	}
	if err := app.RetryHistoryJob(ctx, "job1"); err != nil {
		t.Errorf("RetryHistoryJob() = %v, want nil", err)
	}
	if !app.UnblockServer("srv1") {
		t.Error("UnblockServer() = false, want true")
	}
	if status := app.ServerStatus(); len(status) != 1 || status[0].Name != "srv1" {
		t.Errorf("ServerStatus() = %v, want %v", status, snaps)
	}
	if sp := app.Speed(); sp != 12.34 {
		t.Errorf("Speed() = %v, want 12.34", sp)
	}
	if _, ok := app.DirectUnpackStatus("job1"); ok {
		t.Error("DirectUnpackStatus() ok = true, want false")
	}
	if statuses := app.DirectUnpackStatuses(); statuses != nil {
		t.Errorf("DirectUnpackStatuses() = %v, want nil", statuses)
	}
	if states := app.CheckpointStates(); states != nil {
		t.Errorf("CheckpointStates() = %v on an unconfigured NopApp, want nil", states)
	}
	if st := app.CheckpointState("job1"); st != (appkg.JobCheckpointState{}) {
		t.Errorf("CheckpointState() = %+v on an unconfigured NopApp, want the zero value", st)
	}
	configured := NopApp{CheckpointStatesVal: map[string]appkg.JobCheckpointState{
		"job1": {PendingBytes: 42, StallReason: "Stalled: disk full"},
	}}
	if st := configured.CheckpointState("job1"); st.PendingBytes != 42 || st.StallReason == "" {
		t.Errorf("CheckpointState(job1) = %+v, want the configured figures — the queue detail "+
			"endpoint reads this one, and a stub that dropped them would make its assertions "+
			"pass against a handler that sends nothing", st)
	}
	// ReevaluateStalls must tolerate a nil counter — most tests do not wire
	// one — and must count when one is present, or the R19 "on user action"
	// assertions in internal/api would pass against a handler that never asks.
	app.ReevaluateStalls()
	var calls atomic.Int64
	counting := NopApp{ReevaluatedVal: &calls}
	counting.ReevaluateStalls()
	if got := calls.Load(); got != 1 {
		t.Errorf("ReevaluateStalls calls = %d, want 1", got)
	}

	if cache := app.ArticleCacheBytes(); cache != 0 {
		t.Errorf("ArticleCacheBytes() = %d, want 0", cache)
	}
	if free, err := app.DownloadDirFreeBytes(ctx); free != 0 || err != nil {
		t.Errorf("DownloadDirFreeBytes() = (%d, %v), want (0, nil)", free, err)
	}
	if speed, err := app.TestDownloadDirWriteSpeedMBPerSec(ctx); speed != 0.0 || err != nil {
		t.Errorf("TestDownloadDirWriteSpeedMBPerSec() = (%v, %v), want (0.0, nil)", speed, err)
	}
	if err := app.PingDB(ctx); err != nil {
		t.Errorf("PingDB() = %v, want nil", err)
	}
	if !app.IsPipelineHealthy(ctx) {
		t.Error("IsPipelineHealthy() = false, want true")
	}

	// 2. Safe no-op calls (verify no panic)
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

	// 3. Nil Queue and History safety
	if err := app.AddJob(ctx, nil, nil, false); err != nil {
		t.Errorf("AddJob(nil queue) = %v, want nil", err)
	}
	if err := app.RemoveJob(ctx, "job1", false); err != nil {
		t.Errorf("RemoveJob(nil queue) = %v, want nil", err)
	}
	if err := app.RemoveHistoryJob(ctx, "job1", false); err != nil {
		t.Errorf("RemoveHistoryJob(nil history) = %v, want nil", err)
	}

	// 4. Wired Queue and History delegation
	q := queue.New()
	j, err := queue.NewJob(&nzb.NZB{}, queue.AddOptions{Filename: "job1.nzb", Name: "Test Job"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	j.ID = "job1"
	if err := q.Add(j); err != nil {
		t.Fatalf("q.Add failed: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "hist.db")
	db, err := history.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("history.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	if err := repo.Add(ctx, history.Entry{NzoID: "hjob1", Name: "History Job"}); err != nil {
		t.Fatalf("repo.Add failed: %v", err)
	}

	wiredApp := NopApp{
		Queue:   q,
		History: repo,
	}

	if err := wiredApp.RemoveJob(ctx, "job1", false); err != nil {
		t.Errorf("wired RemoveJob() = %v, want nil", err)
	}
	if job, _ := q.Get("job1"); job != nil {
		t.Error("job1 still in queue after wired RemoveJob()")
	}

	if err := wiredApp.RemoveHistoryJob(ctx, "hjob1", false); err != nil {
		t.Errorf("wired RemoveHistoryJob() = %v, want nil", err)
	}
	if _, err := repo.Get(ctx, "hjob1"); !errors.Is(err, history.ErrNotFound) {
		t.Errorf("hjob1 get err = %v; want history.ErrNotFound", err)
	}
}
