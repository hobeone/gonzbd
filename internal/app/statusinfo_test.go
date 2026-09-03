package app

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/types"
)

var _ = (*Application).enqueuePostProc
var _ = (*pipeline).run

func TestApplication_ArticleCacheBytes_ReturnsZeroInitially(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	if got := app.ArticleCacheBytes(); got != 0 {
		t.Errorf("ArticleCacheBytes() = %d, want 0 on a fresh app", got)
	}
}

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
	app, err := New(cfg, nil)
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
	if err := app.Dispatcher().Add(j, hdr); err != nil {
		t.Fatalf("dispatcher Add: %v", err)
	}

	// Wait for dispatcher tick to transition to Fetching
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if row, ok := app.Dispatcher().Row(j.ID()); ok && row.View.State == job.Fetching {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

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

// TestCheckpointStates_ReportsEveryJobWithAFigureToReport pins the snapshot the
// queue listing reads. It is one pass under each lock rather than one per job
// because a listing is polled continuously, and a job that has any of the three
// figures must appear even when the other two are absent — a job that has
// written bytes but never checkpointed, and one parked before it wrote
// anything, are both real and both invisible if the map were keyed off one
// source.
func TestCheckpointStates_ReportsEveryJobWithAFigureToReport(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.noteJobBytes("wrote-only", 512)
	application.noteBarrierRun("barriered-only")
	application.noteStall("stalled-only", &storagefault.Fault{
		Op: "write", Path: "/data/x.bin", Err: syscall.ENOSPC,
	})

	got := application.CheckpointStates()

	if n := got["wrote-only"].PendingBytes; n != 512 {
		t.Errorf("wrote-only PendingBytes = %d, want 512", n)
	}
	if got["barriered-only"].LastBarrier.IsZero() {
		t.Error("barriered-only has no LastBarrier; a job that has checkpointed but written " +
			"nothing since is missing from the snapshot entirely")
	}
	if r := got["stalled-only"].StallReason; !strings.Contains(r, "no space") {
		t.Errorf("stalled-only StallReason = %q, want it to name the condition", r)
	}
	if _, ok := got["never-seen"]; ok {
		t.Error("a job with nothing to report appears in the snapshot")
	}
}

// TestJobDurability_ReportsDownloadedBytesAsDurable pins the identity the
// listing's bytes_durable rests on: a downloaded byte IS a durable byte, so a
// second counter would be a second representation of one fact, free to drift.
func TestJobDurability_ReportsDownloadedBytesAsDurable(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 3)
	if got := DurableBytesOf(j.Progress()); got != 0 {
		t.Fatalf("fixture already reports %d durable bytes; the assertion below cannot tell "+
			"a real figure from a leaked one", got)
	}
	application.noteJobBytes(j.ID(), 999)

	// Two of the file's three 100-byte articles are on stable storage, as one
	// merged run.
	if err := j.SeedFromRuns([]durability.Run{
		{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 1, Offset: 0, Length: 200},
	}); err != nil {
		t.Fatalf("SeedFromRuns: %v", err)
	}

	got := application.JobDurability(j.ID())

	if got.DurableBytes != 200 {
		t.Errorf("DurableBytes = %d, want 200 — two 100-byte articles are covered by a "+
			"recorded run and the figure does not report them", got.DurableBytes)
	}
	if got.PendingBytes != 999 {
		t.Errorf("PendingBytes = %d, want 999 — the written-but-not-fsynced window is being "+
			"folded into the durable figure or dropped", got.PendingBytes)
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

// TestCheckpointState_ComposesTheThreeSourcesForOneJob pins the single-job form
// the whole-queue snapshot and JobDurability both build on. The three figures
// come from three different maps under two different locks, and a job that has
// only one of them must still report that one.
func TestCheckpointState_ComposesTheThreeSourcesForOneJob(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.noteJobBytes("job-1", 128)
	application.noteBarrierRun("job-1")
	application.noteStall("job-1", &storagefault.Fault{
		Op: "sync", Path: "/data/y.bin", Err: syscall.EIO,
	})

	got := application.CheckpointState("job-1")

	if got.PendingBytes != 128 {
		t.Errorf("PendingBytes = %d, want 128", got.PendingBytes)
	}
	if got.LastBarrier.IsZero() {
		t.Error("LastBarrier is zero although a barrier was recorded")
	}
	if !strings.Contains(got.StallReason, "input/output error") {
		t.Errorf("StallReason = %q, want it to name the condition", got.StallReason)
	}

	if empty := application.CheckpointState("job-2"); empty != (JobCheckpointState{}) {
		t.Errorf("checkpointState for an unknown job = %+v, want the zero value — a listing "+
			"would show every job as stalled", empty)
	}
}
