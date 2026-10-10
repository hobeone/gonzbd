package app

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/types"
)

var _ = (*Application).enqueuePostProc
var _ = (*pipeline).run

func TestApplication_DownloadDirFreeBytes_ReturnsPositiveForRealDir(t *testing.T) {
	t.Parallel()
	dlDir := t.TempDir()
	cfg := testConfig(dlDir, t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	free, err := app.DownloadDirFreeBytes(t.Context())
	if err != nil {
		t.Fatalf("DownloadDirFreeBytes: %v", err)
	}
	if free <= 0 {
		t.Errorf("DownloadDirFreeBytes() = %d, want > 0 for a real temp dir", free)
	}
}

func TestApplication_DownloadDirFreeBytes_NilDiskProbeFallback(t *testing.T) {
	t.Parallel()
	dlDir := t.TempDir()
	appNoProbe := &Application{
		config: testConfig(dlDir, t.TempDir(), t.TempDir()),
	}

	free, err := appNoProbe.DownloadDirFreeBytes(t.Context())
	if err != nil {
		t.Fatalf("DownloadDirFreeBytes: %v", err)
	}
	if free <= 0 {
		t.Errorf("DownloadDirFreeBytes() = %d, want > 0 for a real temp dir", free)
	}
}

func TestApplication_TestDownloadDirWriteSpeedMBPerSec_ReturnsPositive(t *testing.T) {
	t.Parallel()
	dlDir := t.TempDir()
	cfg := testConfig(dlDir, t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	mbPerSec, err := app.TestDownloadDirWriteSpeedMBPerSec(context.Background())
	if err != nil {
		t.Fatalf("TestDownloadDirWriteSpeedMBPerSec: %v", err)
	}
	if mbPerSec <= 0 {
		t.Errorf("mbPerSec = %f, want > 0", mbPerSec)
	}
}

func TestApplication_BinaryVersionsInfo_StableAcrossCalls(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	// Whatever par2/unrar/7z are (or aren't) installed on the test
	// machine, BinaryVersionsInfo() must return the same retained
	// struct every call — proving it reads stored state from New()'s
	// probe rather than re-probing (which would be slow and wasteful)
	// or returning something nondeterministic.
	first := app.BinaryVersionsInfo()
	second := app.BinaryVersionsInfo()
	if first != second {
		t.Errorf("BinaryVersionsInfo() not stable across calls: %+v vs %+v", first, second)
	}
}

func TestApplication_IsPipelineHealthy(t *testing.T) {
	t.Parallel()
	dlDir := t.TempDir()
	cfg := testConfig(dlDir, t.TempDir(), t.TempDir())
	app, err := New(cfg, nil, WithDownloader(downloaderOnly{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()

	// Unstarted app should report unhealthy
	if app.IsPipelineHealthy(ctx) {
		t.Error("expected IsPipelineHealthy=false for unstarted app")
	}

	if err := app.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer app.Shutdown()

	// Idle app with empty dispatcher should report healthy
	if !app.IsPipelineHealthy(ctx) {
		t.Error("expected IsPipelineHealthy=true for started idle app")
	}

	// PingDB should succeed on nil historyRepo
	if err := app.PingDB(ctx); err != nil {
		t.Errorf("PingDB on nil historyRepo: %v", err)
	}

	// Test RecordHeartbeat updates timestamp
	app.RecordHeartbeat()
	if app.lastHeartbeat.Load() <= 0 {
		t.Error("expected lastHeartbeat > 0 after RecordHeartbeat")
	}

	// Test download pipeline stall detection
	j, hdr, _ := BuildIngestJob(app.config, &nzb.NZB{Files: []nzb.File{{
		Subject:  "job1.bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: "a@t", Bytes: 100, Number: 1}},
	}}}, "job1.nzb", types.FetchOptions{NzbName: "job1"}, nil)
	if err := app.Dispatcher().Add(context.Background(), j, hdr); err != nil {
		t.Fatalf("dispatcher Add: %v", err)
	}

	// A precondition, not a nicety: the stalled-pipeline assertion below only
	// means anything while some job is Fetching, because with none
	// IsPipelineHealthy takes its idle branch, refreshes the heartbeat and
	// returns true.
	waitFor(t, func() bool {
		row, ok := app.Dispatcher().Row(j.ID())
		return ok && row.View.State == job.Fetching
	})

	// Set heartbeat to 3 minutes ago
	app.lastHeartbeat.Store(time.Now().Add(-3 * time.Minute).Unix())
	if app.IsPipelineHealthy(ctx) {
		t.Error("expected IsPipelineHealthy=false for stalled download pipeline (>2m heartbeat)")
	}

	// Update heartbeat to now
	app.RecordHeartbeat()
	if !app.IsPipelineHealthy(ctx) {
		t.Error("expected IsPipelineHealthy=true after fresh heartbeat")
	}

	// Paused dispatcher is considered healthy
	app.Dispatcher().Pause()
	if !app.IsPipelineHealthy(ctx) {
		t.Error("expected IsPipelineHealthy=true when dispatcher is paused")
	}
	app.Dispatcher().Resume()

	// App with nil dispatcher is considered healthy when started
	appNoDispatcher := &Application{}
	appNoDispatcher.started.Store(true)
	if !appNoDispatcher.IsPipelineHealthy(ctx) {
		t.Error("expected IsPipelineHealthy=true for started app with nil dispatcher")
	}

	// PingDB with real repository
	histPath := filepath.Join(t.TempDir(), "hist.db")
	histDB, err := history.Open(ctx, histPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(histDB)
	app.historyRepo = repo
	if err := app.PingDB(ctx); err != nil {
		t.Errorf("PingDB with repo: %v", err)
	}
	_ = histDB.Close()
	if err := app.PingDB(ctx); err == nil {
		t.Error("expected PingDB error after DB close, got nil")
	}
}

// TestStallReasons_ReportsEveryParkedJob pins the snapshot the queue listing
// reads: a parked job appears with its reason, and a job that is not parked does
// not appear at all.
func TestStallReasons_ReportsEveryParkedJob(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.noteStall("stalled-only", &storagefault.Fault{
		Op: "write", Path: "/data/x.bin", Err: syscall.ENOSPC,
	}, true)

	got := application.StallReasons()

	if r := got["stalled-only"]; !strings.Contains(r, "no space") {
		t.Errorf("stalled-only StallReason = %q, want it to name the condition", r)
	}
	if _, ok := got["never-seen"]; ok {
		t.Error("a job with nothing to report appears in the snapshot")
	}
}

// TestDurableBytesOf_IsSafeOnAJobWithNoProgress pins the nil case the listing
// can reach: Job.Progress() is nil for a job whose hydration failed, and a
// panic in a queue poll takes the whole API down.
func TestDurableBytesOf_IsSafeOnAJobWithNoProgress(t *testing.T) {
	t.Parallel()
	if got := DurableBytesOf(nil); got != 0 {
		t.Errorf("DurableBytesOf(nil) = %d, want 0", got)
	}
}

type mockProgressCounters struct {
	expected, remaining, failed int64
}

func (m mockProgressCounters) ProgressFigures() (int64, int64, int64) {
	return m.expected, m.remaining, m.failed
}
func (m mockProgressCounters) ExpectedBytes() int64      { return m.expected }
func (m mockProgressCounters) FailedBytes() int64        { return m.failed }
func (m mockProgressCounters) RemainingBytes() int64     { return m.remaining }
func (m mockProgressCounters) ContentFailedBytes() int64 { return m.failed }

func TestDurableBytesOf_ClampsNegativeValues(t *testing.T) {
	t.Parallel()
	// When remaining exceeds expected due to an interleaving or torn read,
	// the result must clamp to 0 rather than reporting negative bytes.
	p := mockProgressCounters{expected: 100, remaining: 200, failed: 0}
	if got := DurableBytesOf(p); got != 0 {
		t.Errorf("DurableBytesOf(torn) = %d, want 0", got)
	}
}
